//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package mmap

type MappedFile struct {
	data []byte
}

func NewMappedFile(data []byte) (*MappedFile, error) {
	return &MappedFile{data: data}, nil
}

func (m *MappedFile) MappedData() []byte {
	return m.data
}

func (m *MappedFile) Close() error {
	return nil
}
