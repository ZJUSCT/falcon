package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	PhasePending      = "Pending"
	PhaseInitializing = "Initializing"
	PhaseSyncing      = "Syncing"
	PhasePublishing   = "Publishing"
	PhaseReady        = "Ready"
	PhasePaused       = "Paused"
	PhaseDegraded     = "Degraded"

	SyncPhaseRunning   = "Running"
	SyncPhaseSucceeded = "Succeeded"
	SyncPhaseFailed    = "Failed"
)

// +kubebuilder:validation:MinProperties=1
type LocalizedString map[string]string

// MirrorInfo carries the public catalog metadata of a mirror. There is no URL
// field on purpose: the public path is always the CR name (publish route
// PathPrefix /<name>, mirrorz entry url <host>/<name>), so same-name CRs can
// never collide and no per-CR URL bookkeeping is needed.
type MirrorInfo struct {
	Name        LocalizedString `json:"name"`
	Description LocalizedString `json:"description"`
	// +kubebuilder:validation:Enum=sync
	Type     string `json:"type"`
	Upstream string `json:"upstream"`
}

// +kubebuilder:validation:XValidation:rule="has(self.configMap) != has(self.secret)",message="exactly one of configMap or secret must be set"
// MirrorInputVolume describes a ConfigMap/Secret volume mounted into the sync
// container. Input volumes are always mounted read-only: the former ReadOnly
// *bool override was removed (the controller hardcodes volumeMount readOnly).
type MirrorInputVolume struct {
	Name      string                        `json:"name"`
	MountPath string                        `json:"mountPath"`
	SubPath   string                        `json:"subPath,omitempty"`
	ConfigMap *corev1.ConfigMapVolumeSource `json:"configMap,omitempty"`
	Secret    *corev1.SecretVolumeSource    `json:"secret,omitempty"`
}

type MirrorSyncSpec struct {
	Interval metav1.Duration `json:"interval"`
	// RetryInterval is the delay before the next synchronization attempt
	// after a *failed* run. It applies while status.consecutiveFailures is
	// below failureRetryLimit; afterwards (and after every success) the
	// regular interval applies again.
	// +kubebuilder:default="15m"
	RetryInterval metav1.Duration `json:"retryInterval"`
	Timeout       metav1.Duration `json:"timeout"`
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	// +kubebuilder:validation:MinItems=1
	Command       []string               `json:"command"`
	Args          []string               `json:"args,omitempty"`
	Env           []corev1.EnvVar        `json:"env,omitempty"`
	EnvFrom       []corev1.EnvFromSource `json:"envFrom,omitempty"`
	Volumes       []MirrorInputVolume    `json:"volumes,omitempty"`
	DataMountPath string                 `json:"dataMountPath,omitempty"`
	// NodeName constrains the Job through the kubernetes.io/hostname node selector.
	// The controller deliberately does not set pod.spec.nodeName because doing so
	// bypasses the scheduler and breaks WaitForFirstConsumer volume binding.
	NodeName     string                      `json:"nodeName,omitempty"`
	NodeSelector map[string]string           `json:"nodeSelector,omitempty"`
	Resources    corev1.ResourceRequirements `json:"resources,omitempty"`
	// FailureRetryLimit caps the fast retry cadence: while
	// status.consecutiveFailures is below this limit a failed run is retried
	// after retryInterval; afterwards the next attempt waits for the regular
	// interval. 0 disables fast retries (failures always wait for interval).
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	FailureRetryLimit int32 `json:"failureRetryLimit"`
	// KeepFailedJobs is the number of newest failed sync Jobs (by creation
	// time) the controller retains for debugging. After every sync run
	// reaches a terminal state, older failed Jobs are deleted (background
	// propagation removes their pods). Succeeded Jobs are untouched: they are
	// pruned with their snapshot generation. 0 keeps no failed Jobs.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	KeepFailedJobs int32 `json:"keepFailedJobs"`
}

type MirrorRetentionSpec struct {
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	PreviousSnapshots int32 `json:"previousSnapshots,omitempty"`
}

type MirrorStorageSpec struct {
	// StorageClassName provisions the stable sync PVC. A Retain policy is
	// appropriate when synchronized data must survive accidental CR deletion.
	StorageClassName string `json:"storageClassName"`
	// ServingStorageClassName provisions disposable snapshot-derived publish
	// PVCs. It must provision from the same storage backend and topology as
	// StorageClassName (under local-PV semantics: the same node), otherwise
	// the VolumeSnapshot `dataSource` clone cannot be provisioned. It
	// normally uses reclaimPolicy: Delete so snapshot pruning reclaims the
	// underlying backend volumes. When omitted, StorageClassName is used.
	ServingStorageClassName string            `json:"servingStorageClassName,omitempty"`
	Capacity                resource.Quantity `json:"capacity"`
	// +kubebuilder:default=ReadWriteOnce
	// +kubebuilder:validation:Enum=ReadWriteOnce;ReadWriteMany;ReadOnlyMany;ReadWriteOncePod
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`
	// VolumeSnapshotClassName snapshots the sync PVC after every successful
	// sync; it must be served by the same storage backend as the
	// StorageClasses above. Required: atomic publication depends on it.
	// +kubebuilder:validation:MinLength=1
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName"`
	// NodeName constrains all volume consumers through the kubernetes.io/hostname node selector.
	NodeName     string              `json:"nodeName,omitempty"`
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Retention    MirrorRetentionSpec `json:"retention,omitempty"`
}

// MirrorServingService describes one publish service of a Mirror: a
// Deployment serving the read-only snapshot clone plus a Service in front of
// it. The service name is the protocol identifier and drives the generated
// children: every entry gets Deployment/Service `<mirror>-publish-<name>`, but
// only the "http" entry additionally gets the publish HTTPRoute.
type MirrorServingService struct {
	// Name is the protocol identifier of this service. Only "http" services
	// are routed through the Gateway API; "rsync" and "git" services are
	// exposed through their Service only (no route, no TCPRoute).
	// +kubebuilder:validation:Enum=http;rsync;git
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3
	Replicas *int32 `json:"replicas,omitempty"`
	// Ports are the container ports declared on the publish container. At
	// least one port is required; the first port is the Service target and is
	// (re)named after the service ("http", "rsync" or "git") so the Service
	// can reference it as a named port.
	// +kubebuilder:validation:MinItems=1
	Ports []corev1.ContainerPort `json:"ports"`
	// Command runs the container; omitted, the image entrypoint is used.
	Command []string `json:"command,omitempty"`
	// Args are appended to the command (or image entrypoint).
	Args []string `json:"args,omitempty"`
	// MountPath is where the data PVC is mounted: the controller mounts it
	// read-only at <MountPath>/<mirror name> (no trailing slash). For
	// "http" services this yields web-root semantics — PVC-root content is
	// served under the /<mirror name> route prefix. rsyncd module paths and
	// git http-backend roots must point at the same directory.
	// +kubebuilder:default=/srv/mirror
	MountPath string `json:"mountPath,omitempty"`
	// +kubebuilder:default=/
	ReadinessPath string                      `json:"readinessPath,omitempty"`
	Resources     corev1.ResourceRequirements `json:"resources,omitempty"`
}

type MirrorSpec struct {
	Paused  bool              `json:"paused,omitempty"`
	Info    MirrorInfo        `json:"info"`
	Sync    MirrorSyncSpec    `json:"sync"`
	Storage MirrorStorageSpec `json:"storage"`
	// Services declares how the active snapshot clone is published. Absent
	// or empty = sync-only mirror: the sync/snapshot pipeline still runs and
	// the publish PVC is still produced, but no publish
	// Deployment/Service/HTTPRoute is created. Service names must be unique
	// (so there can be at most one "http" entry, which owns the publish
	// HTTPRoute).
	// +optional
	// +kubebuilder:validation:XValidation:rule="self.all(s, self.exists_one(e, e.name == s.name))",message="service names must be unique"
	Services []MirrorServingService `json:"services,omitempty"`
}

type MirrorSyncStatus struct {
	JobName string `json:"jobName"`
	// +kubebuilder:validation:Enum=Running;Succeeded;Failed
	Phase      string       `json:"phase"`
	StartedAt  *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	Message    string       `json:"message,omitempty"`
}

type MirrorStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +kubebuilder:validation:Enum=Pending;Initializing;Syncing;Publishing;Ready;Paused;Degraded
	Phase   string `json:"phase,omitempty"`
	WorkPVC string `json:"workPVC,omitempty"`
	// ActivePVC is the name of the publish PVC currently published. Names
	// embed the sync task's start time as a Unix seconds timestamp, e.g.
	// `<mirror>-snap-1756158000`; the timestamp is allocated once when the
	// controller creates the sync task and is shared by the sync Job, the
	// VolumeSnapshot and the publish PVC.
	ActivePVC string `json:"activePVC,omitempty"`
	// ActiveSnapshot is the VolumeSnapshot the ActivePVC was cloned from.
	ActiveSnapshot string `json:"activeSnapshot,omitempty"`
	// PendingSyncTimestamp is the Unix seconds timestamp allocated when the
	// sync task was created. It is embedded in the pending sync Job's name
	// and in the pending snapshot and publish PVC names (which share the
	// same name, `<mirror>-snap-<ts>`).
	PendingSyncTimestamp int64        `json:"pendingSyncTimestamp,omitempty"`
	PendingPVC           string       `json:"pendingPVC,omitempty"`
	PendingSnapshot      string       `json:"pendingSnapshot,omitempty"`
	PendingJob           string       `json:"pendingJob,omitempty"`
	PendingSyncRequest   string       `json:"pendingSyncRequest,omitempty"`
	NextSyncAt           *metav1.Time `json:"nextSyncAt,omitempty"`
	// ConsecutiveFailures counts failed synchronization runs since the last
	// successful publication. It drives the failure retry cadence (retryInterval
	// below failureRetryLimit, interval afterwards) and resets to zero on
	// every success.
	ConsecutiveFailures    int32              `json:"consecutiveFailures,omitempty"`
	LastPublishedAt        *metav1.Time       `json:"lastPublishedAt,omitempty"`
	LastHandledSyncRequest string             `json:"lastHandledSyncRequest,omitempty"`
	SizeBytes              int64              `json:"sizeBytes,omitempty"`
	LastSync               *MirrorSyncStatus  `json:"lastSync,omitempty"`
	Conditions             []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Active PVC",type=string,JSONPath=`.status.activePVC`
// +kubebuilder:printcolumn:name="Last Sync",type=date,JSONPath=`.status.lastSync.finishedAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Mirror struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MirrorSpec   `json:"spec"`
	Status MirrorStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MirrorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Mirror `json:"items"`
}
