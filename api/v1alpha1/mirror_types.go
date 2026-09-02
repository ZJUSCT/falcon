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
	Upstream    string          `json:"upstream"`
}

// MirrorSyncSpec describes one synchronization run. The Job-level knobs
// (interval/retry/timeout/limits), the data mount point and the placement are
// CR fields; everything else about the sync container lives in PodTemplate —
// the full pod template of the sync Job, symmetric to the publish services'
// podTemplate.
type MirrorSyncSpec struct {
	Interval metav1.Duration `json:"interval"`
	// RetryInterval is the delay before the next synchronization attempt
	// after a *failed* run. It applies while status.consecutiveFailures is
	// below failureRetryLimit; afterwards (and after every success) the
	// regular interval applies again.
	// +kubebuilder:default="15m"
	RetryInterval metav1.Duration `json:"retryInterval"`
	Timeout       metav1.Duration `json:"timeout"`
	// DataMountPath is where the controller mounts the WRITABLE sync PVC
	// (volume name `sync-data`, forced) inside the first container. It is the
	// exact mount point. Everything else the sync process needs to know
	// (paths, credentials, tuning) goes through the pod template / explicit
	// env — the controller injects no implicit environment variables.
	//
	// There are no placement fields: sync pods reference the sync PVC, so the
	// scheduler handles locality natively (WaitForFirstConsumer decides the
	// volume's node on first supply; the bound PV's nodeAffinity pins every
	// later sync pod) — see spec/k8s.md.
	DataMountPath string `json:"dataMountPath,omitempty"`
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
	// PodTemplate is the FULL pod template of the sync Job (Job
	// .spec.template): the user declares every container, image, command,
	// args, env, probe, volume and so on — ConfigMap/Secret inputs included,
	// as plain volumes/mounts. The controller forces the sync pipeline
	// identity (the WRITABLE `sync-data` PVC volume mounted at dataMountPath
	// into the first container, restartPolicy Never, the sync labels, the Job
	// deadline) and injects defaults only where the template is silent
	// (restricted-profile security defaults, a /tmp emptyDir, imagePullPolicy
	// IfNotPresent). No placement is injected: volume locality is the
	// scheduler's job.
	// +optional
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate,omitempty"`
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
	// PublishStorageClassName provisions disposable snapshot-derived
	// publish PVCs. It must provision from the same storage backend and
	// topology as StorageClassName (under local-PV semantics: the same node),
	// otherwise the VolumeSnapshot `dataSource` clone cannot be provisioned.
	// It normally uses reclaimPolicy: Delete so snapshot pruning reclaims the
	// underlying backend volumes. When omitted, StorageClassName is used.
	PublishStorageClassName string            `json:"publishStorageClassName,omitempty"`
	Capacity                resource.Quantity `json:"capacity"`
	// +kubebuilder:default=ReadWriteOnce
	// +kubebuilder:validation:Enum=ReadWriteOnce;ReadWriteMany;ReadOnlyMany;ReadWriteOncePod
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`
	// VolumeSnapshotClassName snapshots the sync PVC after every successful
	// sync; it must be served by the same storage backend as the
	// StorageClasses above. Required: atomic publication depends on it.
	// +kubebuilder:validation:MinLength=1
	VolumeSnapshotClassName string              `json:"volumeSnapshotClassName"`
	Retention               MirrorRetentionSpec `json:"retention,omitempty"`
}

// MirrorServiceSpec is one publish service of a Mirror, addressed by a fixed
// key under spec.services ("http" or "rsync"). There is no third "git" key on
// purpose: git publishing is HTTP serving (a fastcgi-style container behind
// the web server), so it is expressed through the "http" key. Each enabled
// service gets a Deployment and a Service named `<mirror>-publish-<key>`; only
// an enabled "http" service additionally gets the publish HTTPRoute (rsync is
// Service-only; a future RsyncRoute is out of scope).
// +kubebuilder:validation:XValidation:rule="!self.enable || (has(self.mirrorMountPath) && self.mirrorMountPath.length() > 0)",message="mirrorMountPath is required when enable is true"
// +kubebuilder:validation:XValidation:rule="!self.enable || has(self.podTemplate.spec)",message="podTemplate.spec is required when enable is true"
type MirrorServiceSpec struct {
	// Enable turns the service on. A key that does not appear in
	// spec.services is disabled, and so is a key declared with
	// enable: false — Enable is the single source of truth.
	Enable bool `json:"enable,omitempty"`
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3
	Replicas *int32 `json:"replicas,omitempty"`
	// MirrorMountPath is where the controller mounts the publish PVC
	// (read-only) inside the first container when the service is enabled. It
	// is the exact mount point: the controller does not append the mirror
	// name (the publish HTTPRoute prefix remains /<mirror name>; point the
	// web root, rsyncd module path or git http-backend root at this
	// directory as the image requires).
	MirrorMountPath string `json:"mirrorMountPath,omitempty"`
	// PodTemplate is the FULL pod template of the publish Deployment
	// (Deployment .spec.template): the user declares every container, port,
	// probe, volume, affinity and so on. The controller forces the
	// data-integrity constraints (the read-only `mirror-data` publish PVC
	// volume mounted at mirrorMountPath, pod labels/annotations, placement,
	// naming/selector identity) and injects defaults only where the template
	// is silent (TCP readiness probe on the first container port, a /tmp
	// emptyDir, readOnlyRootFilesystem and the restricted-profile security
	// defaults).
	// +optional
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate,omitempty"`
}

// MirrorHTTPAlias is one additional public path prefix of an http service.
// Paths are case-sensitive and uppercase is allowed ON PURPOSE: the CR name is
// bound by DNS rules while alias paths are not.
// +kubebuilder:validation:MaxLength=200
type MirrorHTTPAlias string

// MirrorHTTPServiceSpec is the http publish service of a Mirror: the base
// MirrorServiceSpec plus additional public path prefixes (Aliases).
// +kubebuilder:validation:XValidation:rule="!has(self.aliases) || self.aliases.all(a, a.matches('^/([^/\\s]+/)*[^/\\s]+$'))",message="each alias must start with '/', must not end with '/', and must not contain '//' or whitespace"
type MirrorHTTPServiceSpec struct {
	MirrorServiceSpec `json:",inline"`
	// Aliases are ADDITIONAL public path prefixes served by the http service
	// next to the canonical /<mirror name> (e.g. /linux.git and /git/linux.git
	// for the same content — Git smart HTTP is prefix-opaque, so every path
	// serves identical content and negotiation). Aliases are routing-only:
	// the canonical path stays the one true public path (mirrorz output,
	// portal links, documentation). Each alias gets a PathPrefix match on the
	// publish HTTPRoute, appended after the canonical path in declaration
	// order (matches within a rule are OR). Case-sensitive, uppercase
	// allowed; an alias equal to the canonical path is rejected by the
	// controller. Conflicts with other Mirrors' paths are detected at route
	// generation time (RouteConflict condition).
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Aliases []MirrorHTTPAlias `json:"aliases,omitempty"`
}

// MirrorServicesSpec holds the fixed publish service keys of a Mirror. An
// absent key is disabled; a key declared with enable: false is disabled as
// well.
type MirrorServicesSpec struct {
	// HTTP is the HTTP publish service (web server, git http-backend via
	// fastcgi, ...). It owns the publish HTTPRoute when enabled, serving the
	// canonical /<mirror name> path plus any declared aliases.
	HTTP MirrorHTTPServiceSpec `json:"http,omitempty"`
	// Rsync is the rsync publish service. It only gets a Deployment and a
	// ClusterIP Service — no Gateway API route (a future RsyncRoute is out
	// of scope), and no path concept, hence no aliases.
	Rsync MirrorServiceSpec `json:"rsync,omitempty"`
}

// AnyEnabled reports whether at least one publish service key is enabled (a
// mirror with everything disabled syncs but publishes nothing).
func (s MirrorServicesSpec) AnyEnabled() bool {
	return s.HTTP.Enable || s.Rsync.Enable
}

type MirrorSpec struct {
	Paused  bool              `json:"paused,omitempty"`
	Info    MirrorInfo        `json:"info"`
	Sync    MirrorSyncSpec    `json:"sync"`
	Storage MirrorStorageSpec `json:"storage"`
	// Services declares how the active snapshot clone is published, through
	// the fixed keys "http" and "rsync" (see MirrorServicesSpec). With every
	// key disabled (including an entirely absent services object) the mirror
	// is sync-only: the sync/snapshot pipeline still runs and the publish PVC
	// is still produced, but no publish Deployment/Service/HTTPRoute is
	// created.
	// +optional
	Services MirrorServicesSpec `json:"services,omitempty"`
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
