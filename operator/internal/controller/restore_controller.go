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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/aia/mysql-operator/api/v1alpha1"
)

// Vérifie l'intégrité du dump avant de l'appliquer. Avec DATABASES, seules ces bases sont chargées.
const restoreScript = `set -euo pipefail
ARGS=(-h"$MYSQL_HOST" -P"$MYSQL_PORT" -u"$MYSQL_USER")
FILE="` + backupMount + `/$BACKUP_FILE"
[ -f "$FILE" ] || { echo "dump introuvable: $FILE" >&2; exit 1; }
echo "$BACKUP_SHA256  $FILE" | sha256sum -c -
gzip -t "$FILE"
if [ -z "$DATABASES" ]; then
  echo "restauration complète vers $MYSQL_HOST"
  gunzip -c "$FILE" | mysql "${ARGS[@]}"
else
  Q=$(printf '\140')
  for db in $DATABASES; do
    echo "restauration de $db vers $MYSQL_HOST"
    mysql "${ARGS[@]}" -e "CREATE DATABASE IF NOT EXISTS ${Q}${db}${Q}"
    gunzip -c "$FILE" | mysql "${ARGS[@]}" --one-database "$db"
  done
fi
echo "restauration terminée"
`

// RestoreReconciler réconcilie les MySQLRestore.
type RestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=mysql.aia.local,resources=mysqlrestores,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=mysql.aia.local,resources=mysqlrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mysql.aia.local,resources=mysqlbackups,verbs=get;list;watch

func restoreJobName(r *v1.MySQLRestore) string { return "restore-" + r.Name }

func (r *RestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rs v1.MySQLRestore
	if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if rs.Status.Phase == v1.PhaseCompleted || rs.Status.Phase == v1.PhaseFailed {
		return ctrl.Result{}, nil
	}

	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: rs.Namespace, Name: restoreJobName(&rs)}, &job)
	if apierrors.IsNotFound(err) {
		return r.start(ctx, &rs)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	done, ok, msg := jobState(&job)
	switch {
	case !done:
		return ctrl.Result{}, nil
	case !ok:
		return r.finish(ctx, &rs, v1.PhaseFailed, failedMessage(ctx, r.Client, rs.Namespace, job.Name, msg))
	}
	return r.finish(ctx, &rs, v1.PhaseCompleted, "")
}

func (r *RestoreReconciler) start(ctx context.Context, rs *v1.MySQLRestore) (ctrl.Result, error) {
	var b v1.MySQLBackup
	if err := r.Get(ctx, client.ObjectKey{Namespace: rs.Namespace, Name: rs.Spec.BackupName}, &b); err != nil {
		if apierrors.IsNotFound(err) {
			return r.finish(ctx, rs, v1.PhaseFailed, fmt.Sprintf("MySQLBackup %q introuvable", rs.Spec.BackupName))
		}
		return ctrl.Result{}, err
	}
	switch b.Status.Phase {
	case v1.PhaseCompleted:
	case v1.PhaseFailed:
		return r.finish(ctx, rs, v1.PhaseFailed, fmt.Sprintf("MySQLBackup %q en échec, restauration impossible", b.Name))
	default:
		if rs.Status.Phase != v1.PhasePending || rs.Status.Message == "" {
			rs.Status.Phase = v1.PhasePending
			rs.Status.Message = fmt.Sprintf("en attente de la fin du backup %q", b.Name)
			if err := r.Status().Update(ctx, rs); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	env := append(connEnv(rs.Spec.Target),
		corev1.EnvVar{Name: "DATABASES", Value: dbList(rs.Spec.Databases)},
		corev1.EnvVar{Name: "BACKUP_FILE", Value: b.Status.File},
		corev1.EnvVar{Name: "BACKUP_SHA256", Value: b.Status.SHA256},
	)
	j := newJob(rs.Namespace, restoreJobName(rs), imageOr(rs.Spec.Image), storageDefaults(b.Spec.Storage).ClaimName,
		backoff(rs.Spec.BackoffLimit, 0), rs.Spec.Resources, env, restoreScript)
	if err := controllerutil.SetControllerReference(rs, j, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, j); err != nil && !apierrors.IsAlreadyExists(err) {
		return r.finish(ctx, rs, v1.PhaseFailed, "création du Job: "+err.Error())
	}
	now := metav1.Now()
	rs.Status = v1.MySQLRestoreStatus{Phase: v1.PhaseRunning, JobName: j.Name, StartTime: &now}
	return ctrl.Result{}, r.Status().Update(ctx, rs)
}

func (r *RestoreReconciler) finish(ctx context.Context, rs *v1.MySQLRestore, p v1.Phase, msg string) (ctrl.Result, error) {
	now := metav1.Now()
	rs.Status.Phase = p
	rs.Status.Message = msg
	rs.Status.CompletionTime = &now
	return ctrl.Result{}, r.Status().Update(ctx, rs)
}

func (r *RestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.MySQLRestore{}).
		Owns(&batchv1.Job{}).
		Named("mysqlrestore").
		Complete(r)
}
