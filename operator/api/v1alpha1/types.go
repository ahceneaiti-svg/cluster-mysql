package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase décrit l'état d'avancement d'un backup ou d'un restore.
type Phase string

const (
	PhasePending   Phase = "Pending"
	PhaseRunning   Phase = "Running"
	PhaseCompleted Phase = "Completed"
	PhaseFailed    Phase = "Failed"
)

// StorageSpec décrit le PVC qui contient les dumps.
type StorageSpec struct {
	// ClaimName est le nom du PVC (créé par l'opérateur s'il n'existe pas).
	// +kubebuilder:default=mysql-backups
	ClaimName string `json:"claimName,omitempty"`
	// Size est la taille demandée à la création du PVC.
	// +kubebuilder:default="1Gi"
	Size string `json:"size,omitempty"`
	// StorageClassName est la StorageClass du PVC (défaut du cluster si vide).
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// ConnectionSpec décrit comment joindre le serveur MySQL.
type ConnectionSpec struct {
	// Host du serveur MySQL.
	Host string `json:"host"`
	// Port du serveur MySQL.
	// +kubebuilder:default=3306
	Port int32 `json:"port,omitempty"`
	// SecretName contient le mot de passe root.
	// +kubebuilder:default=mysql-secret
	SecretName string `json:"secretName,omitempty"`
	// PasswordKey est la clé du Secret contenant le mot de passe.
	// +kubebuilder:default=MYSQL_ROOT_PASSWORD
	PasswordKey string `json:"passwordKey,omitempty"`
	// User MySQL.
	// +kubebuilder:default=root
	User string `json:"user,omitempty"`
}

// MySQLBackupSpec définit un backup logique (mysqldump) one-shot.
type MySQLBackupSpec struct {
	// Source est le serveur à sauvegarder. Préférer le slave pour ne pas charger le master.
	Source ConnectionSpec `json:"source"`
	// Databases à sauvegarder. Vide = toutes les bases hors schémas système.
	Databases []string    `json:"databases,omitempty"`
	Storage   StorageSpec `json:"storage,omitempty"`
	// Image utilisée par le Job (doit contenir mysqldump).
	// +kubebuilder:default="mysql:8.0"
	Image string `json:"image,omitempty"`
	// BackoffLimit du Job.
	// +kubebuilder:default=2
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`
	// Resources du conteneur de backup.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// MySQLBackupStatus est l'état observé.
type MySQLBackupStatus struct {
	Phase   Phase  `json:"phase,omitempty"`
	JobName string `json:"jobName,omitempty"`
	// File est le chemin du dump relatif à la racine du PVC.
	File           string       `json:"file,omitempty"`
	SizeBytes      int64        `json:"sizeBytes,omitempty"`
	SHA256         string       `json:"sha256,omitempty"`
	Message        string       `json:"message,omitempty"`
	StartTime      *metav1.Time `json:"startTime,omitempty"`
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mbk
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="File",type=string,JSONPath=`.status.file`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.sizeBytes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MySQLBackup déclenche un backup logique.
type MySQLBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MySQLBackupSpec   `json:"spec,omitempty"`
	Status MySQLBackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MySQLBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MySQLBackup `json:"items"`
}

// MySQLRestoreSpec définit la restauration d'un backup.
type MySQLRestoreSpec struct {
	// BackupName est le MySQLBackup (même namespace) à restaurer. Doit être Completed.
	BackupName string `json:"backupName"`
	// Target est le serveur qui reçoit les données (le master, le slave est alimenté par réplication).
	Target ConnectionSpec `json:"target"`
	// Databases à restaurer. Vide = tout le dump.
	Databases []string `json:"databases,omitempty"`
	// Image utilisée par le Job (doit contenir le client mysql).
	// +kubebuilder:default="mysql:8.0"
	Image string `json:"image,omitempty"`
	// BackoffLimit du Job.
	// +kubebuilder:default=0
	BackoffLimit *int32                      `json:"backoffLimit,omitempty"`
	Resources    corev1.ResourceRequirements `json:"resources,omitempty"`
}

type MySQLRestoreStatus struct {
	Phase          Phase        `json:"phase,omitempty"`
	JobName        string       `json:"jobName,omitempty"`
	Message        string       `json:"message,omitempty"`
	StartTime      *metav1.Time `json:"startTime,omitempty"`
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mrs
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=`.spec.backupName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MySQLRestore restaure un MySQLBackup sur un serveur cible.
type MySQLRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MySQLRestoreSpec   `json:"spec,omitempty"`
	Status MySQLRestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MySQLRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MySQLRestore `json:"items"`
}

// MySQLBackupScheduleSpec planifie des backups récurrents.
type MySQLBackupScheduleSpec struct {
	// Schedule au format cron standard (5 champs) ou descripteur (@daily...).
	Schedule string `json:"schedule"`
	// Suspend désactive la planification.
	Suspend bool `json:"suspend,omitempty"`
	// KeepLast nombre de backups Completed conservés (les plus anciens sont supprimés, fichier compris).
	// +kubebuilder:default=7
	// +kubebuilder:validation:Minimum=1
	KeepLast int32 `json:"keepLast,omitempty"`
	// Template du backup créé à chaque échéance.
	Template MySQLBackupSpec `json:"template"`
}

type MySQLBackupScheduleStatus struct {
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`
	LastBackupName   string       `json:"lastBackupName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mbs
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Last",type=date,JSONPath=`.status.lastScheduleTime`
// +kubebuilder:printcolumn:name="Next",type=string,JSONPath=`.status.nextScheduleTime`

// MySQLBackupSchedule crée des MySQLBackup selon un cron.
type MySQLBackupSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MySQLBackupScheduleSpec   `json:"spec,omitempty"`
	Status MySQLBackupScheduleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MySQLBackupScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MySQLBackupSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&MySQLBackup{}, &MySQLBackupList{},
		&MySQLRestore{}, &MySQLRestoreList{},
		&MySQLBackupSchedule{}, &MySQLBackupScheduleList{},
	)
}
