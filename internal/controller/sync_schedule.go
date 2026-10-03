package controller

import (
	"context"
	"hash/fnv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

const (
	// automaticScheduleLead keeps a newly planned run from becoming due in the
	// same reconcile that recorded it.
	automaticScheduleLead = time.Second
	// automaticScheduleGrace distinguishes ordinary queueing delay from a
	// schedule that was missed while a mirror was paused or publishing.
	automaticScheduleGrace = time.Minute
)

// planAutomaticSync assigns a stable phase to mirror and returns its next
// occurrence after now. The fleet's aggregate rate determines the target
// spacing; a stable hash chooses one of the available phase buckets for each
// mirror. This gives every controller restart the same plan without a shared
// scheduler object or a concurrency cap.
func (r *MirrorReconciler) planAutomaticSync(ctx context.Context, mirror *mirrorv1alpha1.Mirror, now time.Time) (time.Time, error) {
	mirrors := &mirrorv1alpha1.MirrorList{}
	if err := r.List(ctx, mirrors, client.InNamespace(mirror.Namespace)); err != nil {
		return time.Time{}, err
	}

	rate := 0.0
	seen := false
	for i := range mirrors.Items {
		item := &mirrors.Items[i]
		if item.Name == mirror.Name {
			seen = true
		}
		if item.Spec.Sync != nil {
			if interval := item.Spec.Sync.Interval.Duration; interval > 0 {
				rate += 1 / interval.Seconds()
			}
		}
	}
	if !seen && mirror.Spec.Sync != nil && mirror.Spec.Sync.Interval.Duration > 0 {
		rate += 1 / mirror.Spec.Sync.Interval.Seconds()
	}
	if rate <= 0 {
		return now.Add(automaticScheduleLead), nil
	}

	spacing := time.Duration(float64(time.Second) / rate)
	if spacing < time.Second {
		spacing = time.Second
	}
	interval := mirror.Spec.Sync.Interval.Duration
	slots := int64(interval / spacing)
	if slots < 1 {
		slots = 1
	}
	bucket := interval / time.Duration(slots)
	phase := time.Duration(hashMirrorName(mirror)%uint64(slots)) * bucket
	phase += bucket / 2
	if phase >= interval {
		phase = interval - time.Nanosecond
	}

	first := time.Unix(0, 0).UTC().Add(phase)
	target := now.UTC().Add(automaticScheduleLead)
	if !first.After(target) {
		periods := target.Sub(first)/interval + 1
		first = first.Add(periods * interval)
	}
	return first, nil
}

func hashMirrorName(mirror *mirrorv1alpha1.Mirror) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(mirror.Namespace + "/" + mirror.Name))
	return h.Sum64()
}

// advanceAutomaticSync keeps an already assigned phase while moving it into
// the future. Missed occurrences are collapsed into one future run.
func advanceAutomaticSync(planned time.Time, interval time.Duration, now time.Time) time.Time {
	if interval <= 0 {
		return planned
	}
	if planned.IsZero() {
		return now.Add(interval)
	}
	if planned.After(now) {
		return planned
	}
	periods := now.Sub(planned)/interval + 1
	return planned.Add(periods * interval)
}
