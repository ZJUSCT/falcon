package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
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

	SyncStateWaiting      = "Waiting"
	SyncStatePending      = SyncPhasePending
	SyncStateSyncing      = "Syncing"
	SyncStateSnapshotting = SyncPhaseSnapshotting
	SyncStateRetrying     = "Retrying"
	SyncStateCancelling   = SyncPhaseCancelling

	SyncRequestAnnotation    = "mirrors.zjusct.io/sync-request"
	AbortRequestAnnotation   = "mirrors.zjusct.io/abort-request"
	RequestCleanupAnnotation = "mirrors.zjusct.io/request-cleanup"
	SyncPhaseCancelling      = "Cancelling"
	SyncPhaseCancelled       = "Cancelled"
	SyncPhasePending         = "Pending"
	SyncPhaseRunning         = "Running"
	SyncPhaseSnapshotting    = "Snapshotting"
	SyncPhaseSucceeded       = "Succeeded"
	SyncPhaseFailed          = "Failed"
)

// MirrorInfo carries the public catalog metadata of a mirror. There is no URL
// field on purpose: the public path is always the CR name (publish route
// PathPrefix /<name>, mirrorz entry url <host>/<name>), so same-name CRs can
// never collide and no per-CR URL bookkeeping is needed.
type MirrorInfo struct {
	// CName is the MirrorZ catalog grouping name, e.g. AOSP. It is not
	// constrained to DNS names; upstream cname.json normalizes known aliases.
	// It defaults to the CR name and does not change the HTTP path.
	// +kubebuilder:validation:MinLength=1
	// +optional
	CName string `json:"cname,omitempty"`
	// Description is plain text for MirrorZ desc; no localization is performed.
	// +optional
	Description string `json:"description,omitempty"`
	Upstream    string `json:"upstream"`
}

// MirrorSyncSpec describes one synchronization run. The Job-level knobs
// (interval/retry/timeout/limits) are CR fields; everything else about the
// sync container lives in PodTemplate — the full pod template of the sync
// Job, symmetric to the publish services' podTemplate. There are no placement
// fields: sync pods reference the sync PVC, so the scheduler handles locality
// natively (WaitForFirstConsumer decides the volume's node on first supply;
// the bound PV's nodeAffinity pins every later sync pod) — see the
// "存储的局部性" (storage locality) section of the documentation.
type MirrorSyncSpec struct {
	// Paused disables automatic synchronization; explicit requests still run.
	// Published content and its serving workloads remain available.
	Paused   bool            `json:"paused,omitempty"`
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
	// KeepJobs is the number of newest sync Jobs (by creation time, any
	// outcome) the controller retains as synchronization history — status
	// inspection and log browsing. After every sync run reaches a terminal
	// state, older Jobs are deleted (background propagation removes their
	// pods, and with them the logs). Jobs are pruned by this count alone:
	// snapshot retention never deletes Jobs, so a Job may outlive its
	// snapshot generation (logs are cheap) or be pruned while its snapshot
	// is still retained (a small count trims history faster than storage).
	// 0 keeps no Jobs.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	KeepJobs int32 `json:"keepJobs"`
	// PodTemplate is the FULL pod template of the sync Job (Job
	// .spec.template): the user declares every container, image, command,
	// args, env, probe, volume and so on — ConfigMap/Secret inputs included,
	// as plain volumes/mounts. Falcon manages the sync pipeline
	// identity (the WRITABLE `sync-data` PVC volume in spec.volumes —
	// mounting it, and where, is the user's own declaration —,
	// restartPolicy Never, the sync labels, and the Job deadline). No workload
	// fields are defaulted or rewritten: security context, filesystem, probes,
	// image policy, environment, and all other inputs are explicit operator
	// declarations.
	// No placement is injected: volume locality is the scheduler's job.
	// +optional
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate,omitempty"`
}

type MirrorStorageSpec struct {
	// PVCSpec contains common Kubernetes PVC properties for the sync and
	// publish claims. storageClassName is managed by the explicit fields
	// below; dataSource, dataSourceRef, and selector are rejected.
	// volumeName is honored by the SYNC claim only: it pre-binds the stable
	// sync PVC to an existing PersistentVolume, e.g. when migrating a mirror
	// across Falcon instances (delete the old Mirror, let its sync PV be
	// retained, clear the PV's claimRef, then pre-bind the new instance's
	// sync PVC here). The publish claims are snapshot clones provisioned
	// through their dataSource and never carry volumeName: a preset
	// volumeName keeps the clone Pending forever (the PV controller only
	// dynamically provisions a claim whose volumeName is empty).
	PVCSpec corev1.PersistentVolumeClaimSpec `json:"pvcTemplate"`
	// SyncStorageClassName provisions the stable writable synchronization PVC.
	SyncStorageClassName string `json:"syncStorageClassName"`
	// PublishStorageClassName provisions disposable snapshot-derived
	// publish PVCs. Required: the publish PVC (a snapshot clone) explicitly
	// uses this StorageClass, an operational choice that is never inherited
	// from SyncStorageClassName. An Immediate-binding class is recommended:
	// under WaitForFirstConsumer the scheduler cannot see the snapshot
	// clone's locality while placing the publish pod (Kubernetes <= 1.36),
	// so the pod may be scheduled to a node where the clone cannot be
	// provisioned — see the "存储的局部性" (storage locality) section of the
	// documentation. It must provision from the same storage backend and
	// topology as SyncStorageClassName (under local-PV semantics: the same
	// node), otherwise the VolumeSnapshot `dataSource` clone cannot be
	// provisioned, and it normally uses reclaimPolicy: Delete so snapshot
	// pruning reclaims the underlying backend volumes.
	// +kubebuilder:validation:MinLength=1
	PublishStorageClassName string `json:"publishStorageClassName"`
	// VolumeSnapshotClassName snapshots the sync PVC after every successful
	// sync; it must be served by the same storage backend as the
	// StorageClasses above. Required: atomic publication depends on it.
	// +kubebuilder:validation:MinLength=1
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName"`
	// Retention counts historical ready snapshots in addition to the latest one,
	// including sync-only generations. Live publication inputs are protected.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	Retention int32 `json:"retention,omitempty"`
}

// MirrorServiceSpec is one publish service of a Mirror, addressed by a fixed
// key under spec.publish ("http" or "rsync"). There is no third "git" key on
// purpose: git publishing uses HTTP (a fastcgi-style container behind
// the web server), so it is expressed through the "http" key. A key that
// appears under spec.publish is ENABLED and gets a Deployment and a Service
// named `<mirror>-publish-<key>`; an absent key is disabled. An enabled
// service must carry a serving podTemplate.spec — CEL-enforced for the "rsync"
// key on MirrorServicesSpec; the "http" key (MirrorHTTPServiceSpec)
// additionally admits a redirect in its place. Only the "http" service
// additionally gets the publish HTTPRoute (rsync is Service-only; a future
// RsyncRoute is out of scope).
type MirrorServiceSpec struct {
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3
	Replicas *int32 `json:"replicas,omitempty"`
	// PodTemplate is the FULL pod template of the publish Deployment
	// (Deployment .spec.template): the user declares every container, port,
	// probe, volume, affinity and so on. Falcon manages the
	// data-integrity constraints (the read-only `mirror-data` publish PVC
	// volume in spec.volumes — mounting it, and where, is the user's own
	// declaration —, pod labels/annotations, naming/selector identity). No
	// workload fields are defaulted or rewritten; the operator owns security
	// context, probes, ports, filesystem, sidecars, and placement (volume
	// locality is the scheduler's job — the bound PV's nodeAffinity).
	// +optional
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate,omitempty"`
}

// MirrorHTTPAlias is one additional public path prefix of an http service.
// Paths are case-sensitive and uppercase is allowed ON PURPOSE: the CR name is
// bound by DNS rules while alias paths are not.
// +kubebuilder:validation:MaxLength=200
type MirrorHTTPAlias string

// MirrorHTTPServiceSpec is the http publish service of a Mirror: the base
// MirrorServiceSpec plus additional public path prefixes (Aliases) and the
// redirect target (Redirect). The key is in SERVING mode when podTemplate.spec
// declares containers: the canonical path and aliases are forwarded to the
// publish Deployment through the publish HTTPRoute, and Redirect is ignored.
// When no serving podTemplate is declared, a set Redirect puts the key in
// REDIRECT mode: no workload is deployed, and the publish HTTPRoute 302-
// redirects every public path (canonical and aliases) to the redirect
// hostname, preserving the request scheme and reusing the request path
// as-is. The temporary-ops intent (e.g. migrating data across nodes) is why
// the status code is 302 and Rsync may keep serving alongside.
// +kubebuilder:validation:XValidation:rule="has(self.podTemplate.spec) || has(self.redirect)",message="podTemplate.spec or redirect is required when the http service key is declared"
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
	// Redirect is the bare hostname every public path of the http service is
	// 302-redirected to in redirect mode (no serving podTemplate declared;
	// a declared podTemplate ignores it). It is a lowercase DNS hostname
	// like the gateway API's PreciseHostname — no scheme, port, or path:
	// the redirect preserves the request scheme (http stays http,
	// https stays https) and reuses the request path unchanged, so every
	// public path maps to the same path on this host.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	Redirect string `json:"redirect,omitempty"`
}

// Serving reports whether the http service is in serving mode: a podTemplate
// declaring at least one container. Nil-safe (an absent key does not serve).
// A spec that bypassed admission may carry a containerless podTemplate.spec
// with no redirect; validateMirror rejects it, and callers treat it as
// non-serving.
func (s *MirrorHTTPServiceSpec) Serving() bool {
	return s != nil && len(s.PodTemplate.Spec.Containers) > 0
}

// RedirectActive reports the redirect target hostname and whether the http
// service is in redirect mode (no serving podTemplate, a non-empty redirect).
// Nil-safe like Serving.
func (s *MirrorHTTPServiceSpec) RedirectActive() (string, bool) {
	if s == nil || s.Serving() || s.Redirect == "" {
		return "", false
	}
	return s.Redirect, true
}

// MirrorServicesSpec holds the fixed publish service keys of a Mirror. An
// absent key is disabled; a present key is enabled — the "rsync" key must
// carry a podTemplate.spec (CEL-enforced), the "http" key must carry either a
// serving podTemplate.spec or a redirect (CEL-enforced on
// MirrorHTTPServiceSpec, shared with ProxyMirror).
// +kubebuilder:validation:XValidation:rule="!has(self.rsync) || has(self.rsync.podTemplate.spec)",message="podTemplate.spec is required when the rsync service key is declared"
type MirrorServicesSpec struct {
	// HTTP is the HTTP publish service (web server, git http-backend via
	// fastcgi, ...). It owns the publish HTTPRoute when enabled, publishing the
	// canonical /<mirror name> path plus any declared aliases — either by
	// forwarding to the publish Deployment (serving mode) or by 302-redirecting
	// every public path to the configured hostname (redirect mode).
	HTTP *MirrorHTTPServiceSpec `json:"http,omitempty"`
	// Rsync is the rsync publish service. It only gets a Deployment and a
	// ClusterIP Service — no Gateway API route (a future RsyncRoute is out
	// of scope), and no path concept, hence no aliases.
	Rsync *MirrorServiceSpec `json:"rsync,omitempty"`
}

// AnyEnabled reports whether at least one publish service requests a
// publication workload — a serving http service or rsync. A redirect-mode
// http service routes without publishing content (no clone PVC, no
// Deployment), so it does not count: the mirror is sync-only next to its
// redirect route.
func (s MirrorServicesSpec) AnyEnabled() bool {
	return s.HTTP.Serving() || s.Rsync != nil
}

type MirrorSpec struct {
	Info    MirrorInfo        `json:"info"`
	Sync    MirrorSyncSpec    `json:"sync"`
	Storage MirrorStorageSpec `json:"storage"`
	// Publish declares how the active snapshot clone is published, through
	// the fixed keys "http" and "rsync" (see MirrorServicesSpec). With every
	// key absent (including an entirely absent services object) the mirror
	// is sync-only: synchronization produces a ready snapshot, without a clone
	// PVC or publish Deployment/Service/HTTPRoute. An http key in redirect
	// mode (no podTemplate, a redirect hostname) deploys nothing either: the
	// publish HTTPRoute 302-redirects the public paths away while
	// synchronization (and any rsync service) continues.
	// +optional
	Publish MirrorServicesSpec `json:"publish,omitempty"`
}

// MirrorSyncStatus records a completed sync Job, independently of publication.
type MirrorSyncStatus struct {
	JobName string `json:"jobName"`
	// +kubebuilder:validation:Enum=Succeeded;Failed;Cancelled
	Phase      string       `json:"phase"`
	StartedAt  *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	Message    string       `json:"message,omitempty"`
}

// MirrorCurrentSyncStatus identifies the accepted transaction. Child names use
// QueuedAt, which stays fixed while the transaction waits for a concurrency slot.
// Phase includes snapshot preparation after the Job; Job timestamps live in LastSync.
type MirrorCurrentSyncStatus struct {
	QueuedAt  *metav1.Time `json:"queuedAt"`
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +kubebuilder:validation:Enum=Pending;Running;Snapshotting;Cancelling
	Phase  string `json:"phase"`
	Manual bool   `json:"manual,omitempty"`
}

// MirrorSyncState describes scheduling/execution independently of publication.
type MirrorSyncState struct {
	// +kubebuilder:validation:Enum=Waiting;Pending;Syncing;Snapshotting;Retrying;Cancelling
	Phase   string `json:"phase"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// MirrorSnapshotStatus records the latest ready output of synchronization,
// including when publication is disabled. It can be published without a new Job.
type MirrorSnapshotStatus struct {
	Name     string       `json:"name"`
	QueuedAt *metav1.Time `json:"queuedAt"`
	JobName  string       `json:"jobName"`
}

// MirrorPublicationStatus consumes a ready snapshot delivered by synchronization.
// Ready resources are recorded incrementally; ActivePVC/ActiveSnapshot continue
// to identify the last successfully served generation until the new one is ready.
type MirrorPublicationStatus struct {
	QueuedAt *metav1.Time `json:"queuedAt"`
	JobName  string       `json:"jobName"`
	// +kubebuilder:validation:Enum=Restoring;RollingOut;Draining
	Phase    string `json:"phase"`
	Snapshot string `json:"snapshot"`
	PVC      string `json:"pvc,omitempty"`
}

// MirrorRequestCleanup is a durable receipt awaiting annotation cleanup.
// The metadata acknowledgement prevents a restart from removing a newer request.
type MirrorRequestCleanup struct {
	Token string `json:"token"`
	Sync  bool   `json:"sync,omitempty"`
	Abort bool   `json:"abort,omitempty"`
}

type MirrorStatus struct {
	RequestCleanup *MirrorRequestCleanup `json:"requestCleanup,omitempty"`
	// LastAcceptedSyncAt reserves a generation second even if its queued run is cancelled.
	LastAcceptedSyncAt *metav1.Time             `json:"lastAcceptedSyncAt,omitempty"`
	LastSnapshot       *MirrorSnapshotStatus    `json:"lastSnapshot,omitempty"`
	Sync               MirrorSyncState          `json:"sync,omitempty"`
	Publication        *MirrorPublicationStatus `json:"publication,omitempty"`
	ObservedGeneration int64                    `json:"observedGeneration,omitempty"`
	// LastAcceptedSpecHash identifies spec.sync accepted for the last
	// synchronization, excluding paused.
	LastAcceptedSpecHash string `json:"lastAcceptedSpecHash,omitempty"`
	WorkPVC              string `json:"workPVC,omitempty"`
	// ActivePVC is the name of the publish PVC currently published. Names
	// embed the transaction's queue entry time as a Unix seconds timestamp, e.g.
	// `<mirror>-snap-1756158000`; the timestamp is allocated once when the
	// controller accepts the sync task and is shared by the sync Job, the
	// VolumeSnapshot and the publish PVC.
	ActivePVC string `json:"activePVC,omitempty"`
	// ActiveSnapshot is the VolumeSnapshot the ActivePVC was cloned from.
	ActiveSnapshot string                   `json:"activeSnapshot,omitempty"`
	CurrentSync    *MirrorCurrentSyncStatus `json:"currentSync,omitempty"`
	NextSyncAt     *metav1.Time             `json:"nextSyncAt,omitempty"`
	// ConsecutiveFailures counts failed synchronization Jobs since the last
	// successful Job. It drives the failure retry cadence (retryInterval
	// below failureRetryLimit, interval afterwards) and resets to zero on
	// every success.
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`
	// LastSuccessfulSyncAt retains the latest successful Job completion for
	// MirrorZ's O token when a later Job is running or has failed.
	// Required before publication; only absent before the first successful Job.
	LastSuccessfulSyncAt *metav1.Time `json:"lastSuccessfulSyncAt,omitempty"`
	// PausedAt is when the controller observed syncing stop under paused=true.
	// Running Jobs drain first; publication proceeds independently.
	PausedAt *metav1.Time `json:"pausedAt,omitempty"`
	// LastAttempt records the last accepted synchronization request outcome,
	// including cancellation before a Job starts. Success means a ready snapshot
	// was delivered; LastSync separately records the Job result.
	LastAttempt     *MirrorSyncStatus  `json:"lastAttempt,omitempty"`
	LastPublishedAt *metav1.Time       `json:"lastPublishedAt,omitempty"`
	SizeBytes       int64              `json:"sizeBytes,omitempty"`
	LastSync        *MirrorSyncStatus  `json:"lastSync,omitempty"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
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

// SyncRequested reports one outstanding manual request. Repeated true writes coalesce.
func (m *Mirror) SyncRequested() bool {
	return m.Annotations[SyncRequestAnnotation] == "true"
}

// AbortRequested applies to the synchronization current when the controller handles it.
func (m *Mirror) AbortRequested() bool {
	return m.Annotations[AbortRequestAnnotation] == "true"
}
