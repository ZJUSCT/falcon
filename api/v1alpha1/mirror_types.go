package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// Phase values are retained as the presentation vocabulary of the legacy
	// /api/jobs endpoint. They are derived from conditions/currentSync and are
	// no longer persisted in CR status.
	PhasePending      = "Pending"
	PhaseInitializing = "Initializing"
	PhaseSyncing      = "Syncing"
	PhasePublishing   = "Publishing"
	PhaseReady        = "Ready"
	PhasePaused       = "Paused"
	PhaseDegraded     = "Degraded"

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
// (interval/retry/timeout/limits) are CR fields; everything else about the
// sync container lives in PodTemplate — the full pod template of the sync
// Job, symmetric to the publish services' podTemplate. There are no placement
// fields: sync pods reference the sync PVC, so the scheduler handles locality
// natively (WaitForFirstConsumer decides the volume's node on first supply;
// the bound PV's nodeAffinity pins every later sync pod) — see docs/spec/k8s.md.
type MirrorSyncSpec struct {
	// Paused prevents the controller from starting new synchronization runs.
	// Published content and its serving workloads remain available.
	Paused bool `json:"paused,omitempty"`
	Interval metav1.Duration `json:"interval"`
	// RetryInterval is the delay before the next synchronization attempt
	// after a *failed* run. It applies while status.consecutiveFailures is
	// below failureRetryLimit; afterwards (and after every success) the
	// regular interval applies again.
	// +kubebuilder:default="15m"
	RetryInterval metav1.Duration `json:"retryInterval"`
	Timeout       metav1.Duration `json:"timeout"`
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
	// identity (the WRITABLE `sync-data` PVC volume in spec.volumes —
	// mounting it, and where, is the user's own declaration —,
	// restartPolicy Never, the sync labels, the Job deadline) and injects
	// defaults only where the template is silent (restricted-profile
	// security defaults, a /tmp emptyDir, imagePullPolicy IfNotPresent). No
	// probes or environment variables are injected: data location and every
	// other input the sync process needs are explicit user declarations.
	// No placement is injected: volume locality is the scheduler's job.
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
// key under spec.publish ("http" or "rsync"). There is no third "git" key on
// purpose: git publishing uses HTTP (a fastcgi-style container behind
// the web server), so it is expressed through the "http" key. A key that
// appears under spec.publish is ENABLED — a valid enable requires podTemplate.spec (CEL-enforced, an empty block is rejected at admission); an absent
// key is disabled. Each enabled service gets a Deployment and a Service named
// `<mirror>-publish-<key>`; only an enabled "http" service additionally gets
// the publish HTTPRoute (rsync is Service-only; a future RsyncRoute is out of
// scope).
// +kubebuilder:validation:XValidation:rule="has(self.podTemplate.spec)",message="podTemplate.spec is required when the service key is declared"
type MirrorServiceSpec struct {
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3
	Replicas *int32 `json:"replicas,omitempty"`
	// PodTemplate is the FULL pod template of the publish Deployment
	// (Deployment .spec.template): the user declares every container, port,
	// probe, volume, affinity and so on. The controller forces the
	// data-integrity constraints (the read-only `mirror-data` publish PVC
	// volume in spec.volumes — mounting it, and where, is the user's own
	// declaration —, pod labels/annotations, placement, naming/selector
	// identity) and injects defaults only where the template is silent (TCP
	// readiness probe on the first container port, a /tmp emptyDir,
	// readOnlyRootFilesystem and the restricted-profile security defaults).
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
	// allowed; the syntax rules and the canonical-path/duplicate rules are
	// enforced by the controller (validateHTTPAliases). Whether the gateway
	// accepts the resulting routes (including precedence against other
	// Mirrors' routes) is the Gateway API's own precedence and acceptance
	// machinery; the controller surfaces an Accepted=False condition as
	// Degraded instead of pre-filtering.
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Aliases []MirrorHTTPAlias `json:"aliases,omitempty"`
}

// MirrorServicesSpec holds the fixed publish service keys of a Mirror. An
// absent key is disabled; a present key — which must carry a valid podTemplate (CEL-enforced); every
// value at its default — is enabled.
type MirrorServicesSpec struct {
	// HTTP is the HTTP publish service (web server, git http-backend via
	// fastcgi, ...). It owns the publish HTTPRoute when enabled, publishing the
	// canonical /<mirror name> path plus any declared aliases.
	HTTP *MirrorHTTPServiceSpec `json:"http,omitempty"`
	// Rsync is the rsync publish service. It only gets a Deployment and a
	// ClusterIP Service — no Gateway API route (a future RsyncRoute is out
	// of scope), and no path concept, hence no aliases.
	Rsync *MirrorServiceSpec `json:"rsync,omitempty"`
}

// AnyEnabled reports whether at least one publish service key is enabled (a
// mirror with everything disabled syncs but publishes nothing).
func (s MirrorServicesSpec) AnyEnabled() bool {
	return s.HTTP != nil || s.Rsync != nil
}

type MirrorSpec struct {
	Info    MirrorInfo        `json:"info"`
	Sync    MirrorSyncSpec    `json:"sync"`
	Storage MirrorStorageSpec `json:"storage"`
	// Publish declares how the active snapshot clone is published, through
	// the fixed keys "http" and "rsync" (see MirrorServicesSpec). With every
	// key absent (including an entirely absent services object) the mirror
	// is sync-only: the sync/snapshot pipeline still runs and the publish PVC
	// is still produced, but no publish Deployment/Service/HTTPRoute is
	// created.
	// +optional
	Publish MirrorServicesSpec `json:"publish,omitempty"`
}

type MirrorSyncStatus struct {
	JobName string `json:"jobName"`
	// +kubebuilder:validation:Enum=Succeeded;Failed
	Phase      string       `json:"phase"`
	StartedAt  *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	Message    string       `json:"message,omitempty"`
}

// MirrorCurrentSyncStatus is the durable identity of the synchronization
// transaction currently in progress. Every child name is derived from the
// Unix seconds of StartedAt, so persisting each deterministic name separately
// would duplicate the same fact.
type MirrorCurrentSyncStatus struct {
	StartedAt   *metav1.Time `json:"startedAt"`
	SyncRequest string       `json:"syncRequest,omitempty"`
}

type MirrorStatus struct {
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	WorkPVC            string `json:"workPVC,omitempty"`
	// ActivePVC is the name of the publish PVC currently published. Names
	// embed the sync task's start time as a Unix seconds timestamp, e.g.
	// `<mirror>-snap-1756158000`; the timestamp is allocated once when the
	// controller creates the sync task and is shared by the sync Job, the
	// VolumeSnapshot and the publish PVC.
	ActivePVC string `json:"activePVC,omitempty"`
	// ActiveSnapshot is the VolumeSnapshot the ActivePVC was cloned from.
	ActiveSnapshot string                   `json:"activeSnapshot,omitempty"`
	CurrentSync    *MirrorCurrentSyncStatus `json:"currentSync,omitempty"`
	NextSyncAt     *metav1.Time             `json:"nextSyncAt,omitempty"`
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
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
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
