package blockdev

import (
	"bytes"
	"fmt"
)

var luksMagic = []byte{'L', 'U', 'K', 'S', 0xba, 0xbe}

// Candidate LUKS imzası taşıyan bir disk/bölüm.
type Candidate struct {
	ID        string `json:"id"`
	Path      string `json:"path"`      // imaj yolu ya da \\.\PhysicalDriveN
	Desc      string `json:"desc"`      // disk modeli vb.
	Partition int    `json:"partition"` // 0 = tüm disk
	PartName  string `json:"partName,omitempty"`
	Offset    int64  `json:"offset"`
	Size      int64  `json:"size"`
}

func (c Candidate) String() string {
	s := c.Path
	if c.Partition > 0 {
		s = fmt.Sprintf("%s bölüm %d", s, c.Partition)
	}
	return s
}

// ScanPath bir aygıtı/imajı açar ve LUKS taşıyan bölümleri (ya da tüm aygıtı) listeler.
func ScanPath(path string) ([]Candidate, error) {
	dev, err := Open(path, false)
	if err != nil {
		return nil, err
	}
	defer dev.Close()
	desc := describe(path)
	var out []Candidate
	if hasLUKS(dev, 0) {
		out = append(out, Candidate{Path: path, Desc: desc, Size: dev.Size()})
		return out, nil
	}
	parts, err := ReadPartitions(dev, dev.Size())
	if err != nil {
		return nil, err
	}
	for _, p := range parts {
		if hasLUKS(dev, p.Offset) {
			out = append(out, Candidate{Path: path, Desc: desc, Partition: p.Number,
				PartName: p.Name, Offset: p.Offset, Size: p.Size})
		}
	}
	return out, nil
}

// ScanAll verilen yolları ve (Windows'ta) tüm fiziksel diskleri tarar.
func ScanAll(paths []string) ([]Candidate, []error) {
	var out []Candidate
	var errs []error
	if len(paths) == 0 {
		paths = physicalDrives()
	}
	for _, p := range paths {
		c, err := ScanPath(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		out = append(out, c...)
	}
	for i := range out {
		out[i].ID = fmt.Sprint(i)
	}
	return out, errs
}

func hasLUKS(dev Device, off int64) bool {
	b := make([]byte, 4096)
	if _, err := dev.ReadAt(b, off); err != nil {
		return false
	}
	return bytes.Equal(b[:6], luksMagic)
}

// OpenCandidate adayın ham aygıtını (bölüm sınırlarıyla) açar.
func OpenCandidate(c Candidate, rw bool) (Device, error) {
	dev, err := Open(c.Path, rw)
	if err != nil {
		return nil, err
	}
	if c.Partition == 0 {
		return dev, nil
	}
	if c.Offset+c.Size > dev.Size() {
		dev.Close()
		return nil, fmt.Errorf("bölüm aygıt sınırını aşıyor")
	}
	return NewSection(dev, c.Offset, c.Size), nil
}
