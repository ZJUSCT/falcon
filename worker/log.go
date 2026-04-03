package worker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/star/mirrorgo/shared"
)

var LogDir string

func CreatLogDir(act *shared.Action) (string, error) {
	dir := filepath.Join(LogDir, act.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}

	return dir, nil
}

func GetLogDir(act *shared.Action) string {
	return filepath.Join(LogDir, act.ID)
}

func MoveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := copyFile(src, dst); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

func copyFile(src, dst string) error {
	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !si.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file: %s", src)
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, si.Mode().Perm())
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()

	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	_ = os.Chtimes(dst, time.Now(), si.ModTime())
	return nil
}
