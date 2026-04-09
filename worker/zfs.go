package worker

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

// zfsCommand runs a zfs/zpool command and returns stdout. Variable for testing.
var zfsCommand = execZFS

func execZFS(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// ---------------------------------------------------------------------------
// Dataset operations
// ---------------------------------------------------------------------------

var datasetProperties = []string{
	"name", "mountpoint", "used", "available", "referenced",
	"logicalused", "logicalreferenced", "compressratio",
	"usedbysnapshots", "usedbydataset", "usedbychildren",
	"written", "creation", "quota", "refquota", "reservation",
	"recordsize", "compression",
}

func ListDatasets(parent string) ([]shared.ZFSDatasetInfo, error) {
	args := []string{"list", "-Hp", "-t", "filesystem",
		"-o", strings.Join(datasetProperties, ",")}
	if parent != "" {
		args = append(args, "-r", parent)
	}
	out, err := zfsCommand("zfs", args...)
	if err != nil {
		return nil, err
	}
	return parseDatasetList(out), nil
}

func parseDatasetList(out string) []shared.ZFSDatasetInfo {
	var datasets []shared.ZFSDatasetInfo
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < len(datasetProperties) {
			continue
		}
		ds := shared.ZFSDatasetInfo{
			Name:              fields[0],
			MountPoint:        dashToEmpty(fields[1]),
			Used:              parseInt64(fields[2]),
			Available:         parseInt64(fields[3]),
			Referenced:        parseInt64(fields[4]),
			LogicalUsed:       parseInt64(fields[5]),
			LogicalReferenced: parseInt64(fields[6]),
			CompressRatio:     fields[7],
			UsedBySnapshots:   parseInt64(fields[8]),
			UsedByDataset:     parseInt64(fields[9]),
			UsedByChildren:    parseInt64(fields[10]),
			Written:           parseInt64(fields[11]),
			Creation:          parseInt64(fields[12]),
			Quota:             parseInt64(fields[13]),
			RefQuota:          parseInt64(fields[14]),
			Reservation:       parseInt64(fields[15]),
			RecordSize:        parseInt64(fields[16]),
			Compression:       fields[17],
		}
		datasets = append(datasets, ds)
	}
	return datasets
}


// ---------------------------------------------------------------------------
// Snapshot operations
// ---------------------------------------------------------------------------

var snapshotProperties = []string{"name", "used", "referenced", "creation"}

func ListSnapshots(dataset string) ([]shared.ZFSSnapshotInfo, error) {
	args := []string{"list", "-Hp", "-t", "snapshot",
		"-o", strings.Join(snapshotProperties, ",")}
	if dataset != "" {
		args = append(args, "-r", dataset)
	}
	out, err := zfsCommand("zfs", args...)
	if err != nil {
		return nil, err
	}
	return parseSnapshotList(out), nil
}

func parseSnapshotList(out string) []shared.ZFSSnapshotInfo {
	var snaps []shared.ZFSSnapshotInfo
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < len(snapshotProperties) {
			continue
		}
		name := fields[0]
		parts := strings.SplitN(name, "@", 2)
		ds, snap := "", name
		if len(parts) == 2 {
			ds = parts[0]
			snap = parts[1]
		}
		snaps = append(snaps, shared.ZFSSnapshotInfo{
			Name:       name,
			Dataset:    ds,
			SnapName:   snap,
			Used:       parseInt64(fields[1]),
			Referenced: parseInt64(fields[2]),
			Creation:   parseInt64(fields[3]),
		})
	}
	return snaps
}

func CreateSnapshot(dataset, snapName string, recursive bool) error {
	args := []string{"snapshot"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, dataset+"@"+snapName)
	_, err := zfsCommand("zfs", args...)
	return err
}

func DestroySnapshot(fullName string) error {
	_, err := zfsCommand("zfs", "destroy", fullName)
	return err
}

// ---------------------------------------------------------------------------
// Dataset CRUD
// ---------------------------------------------------------------------------

func CreateDataset(name string, properties map[string]string) error {
	args := []string{"create"}
	for k, v := range properties {
		args = append(args, "-o", k+"="+v)
	}
	args = append(args, "-p", name)
	_, err := zfsCommand("zfs", args...)
	return err
}

func SetProperty(dataset, property, value string) error {
	_, err := zfsCommand("zfs", "set", property+"="+value, dataset)
	return err
}

// ---------------------------------------------------------------------------
// Pool operations
// ---------------------------------------------------------------------------

var poolProperties = []string{
	"name", "size", "allocated", "free", "fragmentation",
	"capacity", "dedupratio", "health", "altroot",
}

func ListPools() ([]shared.ZFSPoolInfo, error) {
	out, err := zfsCommand("zpool", "list", "-Hp",
		"-o", strings.Join(poolProperties, ","))
	if err != nil {
		return nil, err
	}
	return parsePoolList(out), nil
}

func parsePoolList(out string) []shared.ZFSPoolInfo {
	var pools []shared.ZFSPoolInfo
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < len(poolProperties) {
			continue
		}
		pools = append(pools, shared.ZFSPoolInfo{
			Name:          fields[0],
			Size:          parseInt64(fields[1]),
			Allocated:     parseInt64(fields[2]),
			Free:          parseInt64(fields[3]),
			Fragmentation: dashToEmpty(fields[4]),
			CapacityPct:   dashToEmpty(fields[5]),
			Dedup:         fields[6],
			Health:        fields[7],
			AltRoot:       dashToEmpty(fields[8]),
		})
	}
	return pools
}

// ---------------------------------------------------------------------------
// Periodic report
// ---------------------------------------------------------------------------

func BuildZFSReport(workerName string) shared.ZFSWorkerReport {
	report := shared.ZFSWorkerReport{
		WorkerName: workerName,
		Timestamp:  time.Now().Unix(),
	}

	pools, err := ListPools()
	if err != nil {
		log.Warn().Err(err).Msg("ZFS: failed to list pools")
	} else {
		report.Pools = pools
	}

	datasets, err := ListDatasets("")
	if err != nil {
		log.Warn().Err(err).Msg("ZFS: failed to list datasets")
	} else {
		report.Datasets = datasets
	}

	snapshots, err := ListSnapshots("")
	if err != nil {
		log.Warn().Err(err).Msg("ZFS: failed to list snapshots")
	} else {
		report.Snapshots = snapshots
	}

	return report
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func parseInt64(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "-" || s == "" || s == "none" {
		return 0
	}
	s = strings.TrimSuffix(s, "%")
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

func dashToEmpty(s string) string {
	if s == "-" {
		return ""
	}
	return s
}
