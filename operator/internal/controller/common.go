package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/aia/mysql-operator/api/v1alpha1"
)

const (
	finalizer     = "mysql.aia.local/cleanup"
	scheduleLabel = "mysql.aia.local/schedule"
	backupMount   = "/backup"
)

// jobResult est le message d'arrêt écrit par le script de backup.
type jobResult struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func connDefaults(c v1.ConnectionSpec) v1.ConnectionSpec {
	if c.Port == 0 {
		c.Port = 3306
	}
	if c.SecretName == "" {
		c.SecretName = "mysql-secret"
	}
	if c.PasswordKey == "" {
		c.PasswordKey = "MYSQL_ROOT_PASSWORD"
	}
	if c.User == "" {
		c.User = "root"
	}
	return c
}

func storageDefaults(s v1.StorageSpec) v1.StorageSpec {
	if s.ClaimName == "" {
		s.ClaimName = "mysql-backups"
	}
	if s.Size == "" {
		s.Size = "1Gi"
	}
	return s
}

func imageOr(img string) string {
	if img == "" {
		return "mysql:8.0"
	}
	return img
}

// ensurePVC crée le PVC de stockage s'il n'existe pas.
func ensurePVC(ctx context.Context, c client.Client, ns string, s v1.StorageSpec) error {
	s = storageDefaults(s)
	var pvc corev1.PersistentVolumeClaim
	err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: s.ClaimName}, &pvc)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	qty, err := resource.ParseQuantity(s.Size)
	if err != nil {
		return fmt.Errorf("storage.size invalide: %w", err)
	}
	pvc = corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: s.ClaimName},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: s.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	if err := c.Create(ctx, &pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// newJob construit un Job mono-pod qui exécute script avec le PVC monté sur /backup.
func newJob(ns, name, image, claim string, backoff *int32, res corev1.ResourceRequirements, env []corev1.EnvVar, script string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "mysql-operator"}},
		Spec: batchv1.JobSpec{
			BackoffLimit: backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:                     "worker",
						Image:                    image,
						Command:                  []string{"bash", "-c", script},
						Env:                      env,
						Resources:                res,
						VolumeMounts:             []corev1.VolumeMount{{Name: "backup", MountPath: backupMount}},
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
					}},
					Volumes: []corev1.Volume{{
						Name: "backup",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
						},
					}},
				},
			},
		},
	}
}

func connEnv(c v1.ConnectionSpec) []corev1.EnvVar {
	c = connDefaults(c)
	return []corev1.EnvVar{
		{Name: "MYSQL_HOST", Value: c.Host},
		{Name: "MYSQL_PORT", Value: fmt.Sprint(c.Port)},
		{Name: "MYSQL_USER", Value: c.User},
		{Name: "MYSQL_PWD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: c.SecretName}, Key: c.PasswordKey,
		}}},
	}
}

// jobState renvoie (terminé, réussi, message) d'après les conditions du Job.
func jobState(j *batchv1.Job) (done, ok bool, msg string) {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, true, ""
		case batchv1.JobFailed:
			return true, false, strings.TrimSpace(c.Reason + ": " + c.Message)
		}
	}
	return false, false, ""
}

// jobTerminationMessage lit le message d'arrêt du pod du Job (dernier pod terminé).
func jobTerminationMessage(ctx context.Context, c client.Client, ns, job string) string {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{"job-name": job}); err != nil {
		return ""
	}
	var msg string
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if t := cs.State.Terminated; t != nil && t.Message != "" {
				msg = t.Message
			}
		}
	}
	return msg
}

func parseResult(msg string) (jobResult, bool) {
	var r jobResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(msg)), &r); err != nil {
		return r, false
	}
	return r, true
}

// failedMessage complète le message de Job avec la fin du message d'arrêt éventuel.
func failedMessage(ctx context.Context, c client.Client, ns, job, base string) string {
	if m := jobTerminationMessage(ctx, c, ns, job); m != "" && !strings.HasPrefix(strings.TrimSpace(m), "{") {
		return strings.TrimSpace(m)
	}
	return base
}

func backoff(p *int32, def int32) *int32 {
	if p != nil {
		return p
	}
	return ptr.To(def)
}

func dbList(dbs []string) string { return strings.Join(dbs, " ") }
