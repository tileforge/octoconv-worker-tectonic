package convert

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// ErrJobFileIrregular means the path is not a regular file.
var ErrJobFileIrregular = errors.New("not a regular file")

// OpenJobFile opens path only when it is a regular file and not a symlink.
func OpenJobFile(path string) (*os.File, fs.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, nil, fmt.Errorf("%w: %s is a symbolic link", ErrJobFileIrregular, path)
	}
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%w: %s is %v", ErrJobFileIrregular, path, info.Mode().Type())
	}
	return f, info, nil
}
