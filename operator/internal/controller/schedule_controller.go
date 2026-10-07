package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/aia/mysql-operator/api/v1alpha1"
)

// ScheduleReconciler réconcilie les MySQLBackupSchedule.
type ScheduleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=mysql.aia.local,resources=mysqlbackupschedules,verbs=get;list;watch
// +kubebuilder:rbac:groups=mysql.aia.local,resources=mysqlbackupschedules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mysql.aia.local,resources=mysqlbackups,verbs=create;delete

func (r *ScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var s v1.MySQLBackupSchedule
	if err := r.Get(ctx, req.NamespacedName, &s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	sched, err := cron.ParseStandard(s.Spec.Schedule)
	if err != nil {
		return ctrl.Result{}, nil // spec invalide: inutile de réessayer avant modification
	}

	if err := r.prune(ctx, &s); err != nil {
		return ctrl.Result{}, err
	}
	if s.Spec.Suspend {
		if s.Status.NextScheduleTime != nil {
			s.Status.NextScheduleTime = nil
			return ctrl.Result{}, r.Status().Update(ctx, &s)
		}
		return ctrl.Result{}, nil
	}

	now := time.Now()
	last := s.CreationTimestamp.Time
	if s.Status.LastScheduleTime != nil {
		last = s.Status.LastScheduleTime.Time
	}
	// Dernière échéance dépassée (les échéances manquées sont regroupées en une seule).
	var due time.Time
	for t := sched.Next(last); !t.After(now); t = sched.Next(t) {
		due = t
	}
	if !due.IsZero() {
		name := fmt.Sprintf("%s-%s", s.Name, due.UTC().Format("20060102-150405"))
		b := &v1.MySQLBackup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: s.Namespace, Name: name,
				Labels: map[string]string{scheduleLabel: s.Name},
			},
			Spec: *s.Spec.Template.DeepCopy(),
		}
		if err := controllerutil.SetControllerReference(&s, b, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, b); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		t := metav1.NewTime(due)
		s.Status.LastScheduleTime = &t
		s.Status.LastBackupName = name
		last = due
	}
	next := metav1.NewTime(sched.Next(last))
	s.Status.NextScheduleTime = &next
	if err := r.Status().Update(ctx, &s); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Until(next.Time) + time.Second}, nil
}

// prune supprime les backups Completed au-delà de KeepLast (le finalizer supprime les fichiers).
func (r *ScheduleReconciler) prune(ctx context.Context, s *v1.MySQLBackupSchedule) error {
	keep := int(s.Spec.KeepLast)
	if keep < 1 {
		keep = 7
	}
	var list v1.MySQLBackupList
	if err := r.List(ctx, &list, client.InNamespace(s.Namespace), client.MatchingLabels{scheduleLabel: s.Name}); err != nil {
		return err
	}
	var done []v1.MySQLBackup
	for _, b := range list.Items {
		if b.Status.Phase == v1.PhaseCompleted && b.DeletionTimestamp.IsZero() {
			done = append(done, b)
		}
	}
	if len(done) <= keep {
		return nil
	}
	sort.Slice(done, func(i, j int) bool { return done[i].CreationTimestamp.Before(&done[j].CreationTimestamp) })
	for _, b := range done[:len(done)-keep] {
		if err := r.Delete(ctx, &b); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func (r *ScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.MySQLBackupSchedule{}).
		Owns(&v1.MySQLBackup{}).
		Named("mysqlbackupschedule").
		Complete(r)
}
