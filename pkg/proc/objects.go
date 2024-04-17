package proc

const heapInfoSize = 512

// Information for heapInfoSize bytes of heap.
type heapInfo struct {
	base     Address // start of the span containing this heap region
	size     int64   // size of objects in the span
	mark     uint64  // 64 mark bits, one for every 8 bytes
	firstIdx int     // the index of the first object that starts in this region, or -1 if none
	// For 64-bit inferiors, ptr[0] contains 64 pointer bits, one
	// for every 8 bytes.  On 32-bit inferiors, ptr contains 128
	// pointer bits, one for every 4 bytes.
	ptr [2]uint64
}

func (h *heapInfo) IsPtr(a Address, ptrSize int64) bool {
	if ptrSize == 8 {
		i := uint(a%heapInfoSize) / 8
		return h.ptr[0]>>i&1 != 0
	}
	i := a % heapInfoSize / 4
	return h.ptr[i/64]>>(i%64)&1 != 0
}

// setHeapPtr records that the memory at heap address a contains a pointer.
func (t *Target) setHeapPtr(a Address) {
	h := t.allocHeapInfo(a)
	if t.BinInfo().Arch.PtrSize() == 8 {
		i := uint(a%heapInfoSize) / 8
		h.ptr[0] |= uint64(1) << i
		return
	}
	i := a % heapInfoSize / 4
	h.ptr[i/64] |= uint64(1) << (i % 64)
}

// Heap info structures cover 9 bits of address.
// A page table entry covers 20 bits of address (1MB).
const pageTableSize = 1 << 11

type pageTableEntry [pageTableSize]heapInfo

// findHeapInfo finds the heapInfo structure for a.
// Returns nil if a is not a heap address.
func (t *Target) findHeapInfo(a Address) *heapInfo {
	k := a / heapInfoSize / pageTableSize
	i := a / heapInfoSize % pageTableSize
	pt := t.pageTable[k]
	if pt == nil {
		return nil
	}
	h := &pt[i]
	if h.base == 0 {
		return nil
	}
	return h
}

// Same as findHeapInfo, but allocates the heapInfo if it
// hasn't been allocated yet.
func (t *Target) allocHeapInfo(a Address) *heapInfo {
	k := a / heapInfoSize / pageTableSize
	i := a / heapInfoSize % pageTableSize
	pt := t.pageTable[k]
	if pt == nil {
		pt = new(pageTableEntry)
		for j := 0; j < pageTableSize; j++ {
			pt[j].firstIdx = -1
		}
		t.pageTable[k] = pt
		t.pages = append(t.pages, k)
	}
	return &pt[i]
}
