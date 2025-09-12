//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package mmap

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// MappedFile wraps a memory mapped file.
type MappedFile struct {
	data []byte // memory mapped data
	file *os.File
}

// NewMappedFile creates a new memory mapped file.
func NewMappedFile(data []byte) (*MappedFile, error) {
	dataSize := int64(len(data))
	tmpFile, err := os.CreateTemp("", "delve_dwarf_mmap_*")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(tmpFile.Name()) // delete from the directory, but keep the file descriptor open

	if err = tmpFile.Truncate(dataSize); err != nil {
		_ = tmpFile.Close()
		return nil, err
	}

	_, err = tmpFile.Write(data)
	if err != nil {
		return nil, fmt.Errorf("could not write data to mapped file, err=%v", err)
	}

	mappedData, err := unix.Mmap(int(tmpFile.Fd()), 0, int(dataSize),
		unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		_ = tmpFile.Close()
		return nil, err
	}

	// start a goroutine to periodically call madvise on the mapped data to reduce memory usage
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := unix.Madvise(mappedData, unix.MADV_DONTNEED); err != nil {
					panic(fmt.Sprintf("call madvise MADV_DONTNEED on mapped data failed: %v", err))
				}
			}
		}
	}()

	return &MappedFile{
		data: mappedData,
		file: tmpFile,
	}, nil
}

func (m *MappedFile) MappedData() []byte {
	return m.data
}

// Close releases the resources.
func (m *MappedFile) Close() error {
	_ = unix.Munmap(m.data)
	return m.file.Close()
}
