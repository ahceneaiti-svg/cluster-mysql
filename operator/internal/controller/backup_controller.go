package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/aia/mysql-operator/api/v1alpha1"
)

// Le dump est écrit dans un fichier .partial puis renommé : un fichier final existe => dump valide.
const backupScript = `set -euo pipefail
ARGS=(-h"$MYSQL_HOST" -P"$MYSQL_PORT" -u"$MYSQL_USER")
DBS="$DATABASES"
if [ -z "$DBS" ]; then
  DBS=$(mysql "${ARGS[@]}" -N -e 'SHOW DATABASES' | grep -Ev '^(information_schema|performance_schema|mysql|sys)$' | tr '\n' ' ')
fi
[ -n "${DBS// /}" ] || { echo "aucune base utilisateur à sauvegarder" >&2; exit 1; }
FILE="` + backupMount + `/$BACKUP_FILE"
mkdir -p "$(dirname "$FILE")"
echo "dump de: $DBS depuis $MYSQL_HOST"
# set-gtid-purged=OFF : le dump reste restaurable sur un serveur dont gtid_executed n'est pas vide
mysqldump "${ARGS[@]}" --single-transaction --routines --triggers --events --set-gtid-purged=OFF --databases $DBS | gzip > "$FILE.partial"
gzip -t "$FILE.partial"
mv "$FILE.partial" "$FILE"
SUM=$(sha256sum "$FILE" | cut -d' ' -f1)
SIZE=$(stat -c %s "$FILE")
printf '{"size":%s,"sha256":"%s"}' "$SIZE" "$SUM" > /dev/termination-log
echo "ok $FILE $SIZE octets sha256=$SUM"
`

const cleanupScript = `rm -f "` + backupMount + `/$BACKUP_FILE" "` + backupMount + `/$BACKUP_FILE.partial"; echo supprimé`

// BackupReconciler réconcilie les MySQLBackup.
type BackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=mysql.aia.local,resources=mysqlbackups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=mysql.aia.local,resources=mysqlbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mysql.aia.local,resources=mysqlbackups/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

func backupFile(b *v1.MySQLBackup) string    { return fmt.Sprintf("%s/%s.sql.gz", b.Namespace, b.Name) }
func backupJobName(b *v1.MySQLBackup) string { return "backup-" + b.Name }

func (r *BackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b v1.MySQLBackup
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !b.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &b)
	}
	if controllerutil.AddFinalizer(&b, finalizer) {
		if err := r.Update(ctx, &b); err != nil {
			return ctrl.Result{}, err
		}
	}
	if b.Status.Phase == v1.PhaseCompleted || b.Status.Phase == v1.PhaseFailed {
		return ctrl.Result{}, nil
	}

	if err := ensurePVC(ctx, r.Client, b.Namespace, b.Spec.Storage); err != nil {
		return r.fail(ctx, &b, err.Error())
	}

	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: backupJobName(&b)}, &job)
	if apierrors.IsNotFound(err) {
		env := append(connEnv(b.Spec.Source),
			corev1.EnvVar{Name: "DATABASES", Value: dbList(b.Spec.Databases)},
			corev1.EnvVar{Name: "BACKUP_FILE", Value: backupFile(&b)},
		)
		j := newJob(b.Namespace, backupJobName(&b), imageOr(b.Spec.Image), storageDefaults(b.Spec.Storage).ClaimName,
			backoff(b.Spec.BackoffLimit, 2), b.Spec.Resources, env, backupScript)
		if err := controllerutil.SetControllerReference(&b, j, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, j); err != nil && !apierrors.IsAlreadyExists(err) {
			return r.fail(ctx, &b, "création du Job: "+err.Error())
		}
		now := metav1.Now()
		b.Status = v1.MySQLBackupStatus{Phase: v1.PhaseRunning, JobName: j.Name, File: backupFile(&b), StartTime: &now}
		return ctrl.Result{}, r.Status().Update(ctx, &b)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	done, ok, msg := jobState(&job)
	switch {
	case !done:
		return ctrl.Result{}, nil
	case !ok:
		return r.fail(ctx, &b, failedMessage(ctx, r.Client, b.Namespace, job.Name, msg))
	}
	res, found := parseResult(jobTerminationMessage(ctx, r.Client, b.Namespace, job.Name))
	if !found {
		return r.fail(ctx, &b, "résultat du backup introuvable (message d'arrêt vide)")
	}
	now := metav1.Now()
	b.Status.Phase = v1.PhaseCompleted
	b.Status.SizeBytes = res.Size
	b.Status.SHA256 = res.SHA256
	b.Status.CompletionTime = &now
	b.Status.Message = ""
	return ctrl.Result{}, r.Status().Update(ctx, &b)
}

func (r *BackupReconciler) fail(ctx context.Context, b *v1.MySQLBackup, msg string) (ctrl.Result, error) {
	now := metav1.Now()
	b.Status.Phase = v1.PhaseFailed
	b.Status.Message = msg
	b.Status.CompletionTime = &now
	return ctrl.Result{}, r.Status().Update(ctx, b)
}

// finalize supprime le fichier de dump du PVC via un Job avant de libérer l'objet.
func (r *BackupReconciler) finalize(ctx context.Context, b *v1.MySQLBackup) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(b, finalizer) {
		return ctrl.Result{}, nil
	}
	release := func() (ctrl.Result, error) {
		controllerutil.RemoveFinalizer(b, finalizer)
		return ctrl.Result{}, r.Update(ctx, b)
	}
	// Aucun Job lancé => aucun fichier.
	if b.Status.Phase == "" {
		return release()
	}
	claim := storageDefaults(b.Spec.Storage).ClaimName
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: claim}, &pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return release()
		}
		return ctrl.Result{}, err
	}
	// Ne pas lancer le nettoyage en concurrence avec le dump en cours.
	var bj batchv1.Job
	if err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: backupJobName(b)}, &bj); err == nil {
		if done, _, _ := jobState(&bj); !done {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}
	name := "cleanup-" + b.Name
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, &job)
	if apierrors.IsNotFound(err) {
		j := newJob(b.Namespace, name, imageOr(b.Spec.Image), claim, ptr.To[int32](2), corev1.ResourceRequirements{},
			[]corev1.EnvVar{{Name: "BACKUP_FILE", Value: backupFile(b)}}, cleanupScript)
		j.Spec.TTLSecondsAfterFinished = ptr.To[int32](300)
		if err := r.Create(ctx, j); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	done, ok, msg := jobState(&job)
	if !done {
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}
	if !ok {
		return ctrl.Result{}, fmt.Errorf("nettoyage du dump en échec: %s", msg)
	}
	return release()
}

func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.MySQLBackup{}).
		Owns(&batchv1.Job{}).
		Named("mysqlbackup").
		Complete(r)
}
