package zfsagent

import (
	"strconv"
	"strings"
)

// datasetProps accumulates the raw zfs properties of one dataset. The
// dataset's snapshots are nested under snapshots, keyed by their full ZFS
// name (<dataset>@<snapshot>).
type datasetProps struct {
	used       int64
	referenced int64
	written    int64
	creation   int64

	pvcName      string
	pvcNamespace string
	vsName       string
	vsNamespace  string

	snapshots map[string]*datasetProps
}

// parseZfsGet parses the output of
//
//	zfs get -r -Hp -t filesystem,volume,snapshot -o name,property,value <props> <pool>
//
// -Hp emits tab-separated name/property/value triples with numeric properties
// as bare integers (no units, no header row). Robustness rules:
//
//   - lines that are not well-formed triples are skipped;
//   - a value of "-" (and unknown properties) counts as unset;
//   - unparsable numeric values count as unset — the report is best-effort
//     usage accounting, not an audit;
//   - lines whose name contains '@' are snapshot rows and are attached to
//     their dataset (the part before the first '@'); the dataset row itself
//     may be absent, in which case an empty entry is created so the snapshot
//     is not lost.
func parseZfsGet(out []byte) map[string]*datasetProps {
	props := map[string]*datasetProps{}
	entry := func(name string) *datasetProps {
		p := props[name]
		if p == nil {
			p = &datasetProps{}
			props[name] = p
		}
		return p
	}

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		name, property, value := fields[0], fields[1], fields[2]

		// Both dataset and snapshot names exclude '@', so the first '@'
		// always splits dataset from snapshot.
		if parent, _, found := strings.Cut(name, "@"); found {
			p := entry(parent)
			if p.snapshots == nil {
				p.snapshots = map[string]*datasetProps{}
			}
			sp := p.snapshots[name]
			if sp == nil {
				sp = &datasetProps{}
				p.snapshots[name] = sp
			}
			setProp(sp, property, value)
			continue
		}
		setProp(entry(name), property, value)
	}
	return props
}

// setProp applies one name/property/value line to p. Values of "-" mean "not
// set" (zfs prints it for properties that do not apply); numeric values that
// do not parse are ignored.
func setProp(p *datasetProps, property, value string) {
	switch property {
	case "used":
		if v, ok := parseZfsInt(value); ok {
			p.used = v
		}
	case "referenced":
		if v, ok := parseZfsInt(value); ok {
			p.referenced = v
		}
	case "written":
		if v, ok := parseZfsInt(value); ok {
			p.written = v
		}
	case "creation":
		if v, ok := parseZfsInt(value); ok {
			p.creation = v
		}
	case "openebs.io:pvc-name":
		p.pvcName = unsetDash(value)
	case "openebs.io:pvc-namespace":
		p.pvcNamespace = unsetDash(value)
	case "openebs.io:vs-name":
		p.vsName = unsetDash(value)
	case "openebs.io:vs-namespace":
		p.vsNamespace = unsetDash(value)
	}
	// Unknown properties (newer zfs versions, other userprops) are ignored.
}

func unsetDash(value string) string {
	if value == "-" {
		return ""
	}
	return value
}

// parseZfsInt parses a -Hp numeric property value. -Hp already reports plain
// integers (no unit suffixes), so no size parsing is needed.
func parseZfsInt(value string) (int64, bool) {
	if value == "" || value == "-" {
		return 0, false
	}
	v, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
