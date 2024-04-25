package proc

import (
	"fmt"
	"reflect"

	"github.com/go-delve/delve/pkg/dwarf/godwarf"
	"github.com/go-delve/delve/pkg/logflags"
)

type ReferenceVariable struct {
	Addr     uint64
	Name     string
	RealType godwarf.Type

	Children []*ReferenceVariable

	// all referenced object size
	allSize int64
	// all referenced object count
	allCount int64
}

type ObjRefScope struct {
	*HeapScope

	gr           *G
	stackVisited map[Address]bool
}

// todo:
// 1. mark allSize and allCount
// 2. 去重
// 3. 尽量识别所有能识别的类型
// real type
// 栈变量循环引用怎么办？
func (s *ObjRefScope) findObject(addr Address, typ godwarf.Type) (v *ReferenceVariable) {
	h := s.findHeapInfo(addr)
	if h == nil {
		// Not in Go heap
		if s.gr == nil || uint64(addr) < s.gr.stack.lo || uint64(addr) > s.gr.stack.hi {
			// Not in Go stack
			return nil
		}
		if s.stackVisited[addr] {
			return nil
		}
		s.stackVisited[addr] = true
		v = &ReferenceVariable{
			Addr:     uint64(addr),
			RealType: resolveTypedef(typ),
		}
		return v
	}
	x := h.base.Add(addr.Sub(h.base) / h.size * h.size)
	// Check if object is marked.
	h = s.findHeapInfo(x)
	// Find mark bit
	b := uint64(x) % heapInfoSize / 8
	if h.mark&(uint64(1)<<b) != 0 { // already found
		return nil
	}
	h.mark |= uint64(1) << b
	v = &ReferenceVariable{
		Addr:     uint64(addr),
		RealType: resolveTypedef(typ),
	}
	v.allSize += h.size
	v.allCount += 1
	// mark each unknown type pointer
	for i := int64(0); i < h.size; i += int64(s.bi.Arch.PtrSize()) {
		a := x.Add(i)
		// explicit traversal known type
		if a >= addr && a < addr.Add(typ.Size()) {
			continue
		}
		if !s.isPtrFromHeap(a) {
			continue
		}
		ptr, _ := readUintRaw(s.mem, uint64(a), int64(s.bi.Arch.PtrSize()))
		if ptr > 0 {
			if sv := s.findObject(Address(ptr), &godwarf.VoidType{CommonType: godwarf.CommonType{ByteSize: int64(0)}}); sv != nil {
				v.allSize += sv.allSize
				v.allCount += sv.allCount
			}
		}
	}
	return v
}

func (s *ObjRefScope) directBucketObject(addr Address, typ godwarf.Type) (v *ReferenceVariable) {
	v = &ReferenceVariable{
		Addr:     uint64(addr),
		RealType: resolveTypedef(typ),
	}
	v.allSize += typ.Size()
	return v
}

func (s *ObjRefScope) fillRefs(x *ReferenceVariable) {
	switch typ := x.RealType.(type) {
	case *godwarf.PtrType:
		ptrval, _ := readUintRaw(s.mem, x.Addr, int64(s.bi.Arch.PtrSize()))
		if ptrval != 0 {
			if y := s.findObject(Address(ptrval), resolveTypedef(typ.Type)); y != nil {
				s.fillRefs(y)
				// flatten reference
				x.Children = y.Children
				x.allSize += y.allSize
				x.allCount += y.allCount
			}
		}
	case *godwarf.VoidType:
		return
	case *godwarf.ChanType:
		ptrval, _ := readUintRaw(s.mem, x.Addr, int64(s.bi.Arch.PtrSize()))
		if ptrval != 0 {
			if y := s.findObject(Address(ptrval), resolveTypedef(typ.Type.(*godwarf.PtrType).Type)); y != nil {
				x.allSize += y.allSize
				x.allCount += y.allCount

				structType, ok := y.RealType.(*godwarf.StructType)
				if !ok {
					logflags.DebuggerLogger().Errorf("bad channel type %v", y.RealType.String())
					return
				}
				chanLen, _ := readUintRaw(s.mem, uint64(Address(x.Addr).Add(structType.Field[1].ByteOffset)), structType.Field[1].ByteSize)

				if chanLen > 0 {
					for _, field := range structType.Field {
						if field.Name == "buf" {
							zptrval, _ := readUintRaw(s.mem, uint64(Address(y.Addr).Add(field.ByteOffset)), int64(s.bi.Arch.PtrSize()))
							if zptrval != 0 {
								if z := s.findObject(Address(zptrval), fakeArrayType(chanLen, typ.ElemType)); z != nil {
									s.fillRefs(z)
									x.Children = z.Children
									x.allSize += z.allSize
									x.allCount += z.allCount
								}
							}
							break
						}
					}
				}
			}
		}
	case *godwarf.MapType:
		ptrval, _ := readUintRaw(s.mem, x.Addr, int64(s.bi.Arch.PtrSize()))
		if ptrval != 0 {
			if y := s.findObject(Address(ptrval), resolveTypedef(typ.Type.(*godwarf.PtrType).Type)); y != nil {
				x.allSize += y.allSize
				x.allCount += y.allCount

				xv := newVariable("", x.Addr, x.RealType, s.bi, s.mem)
				it := xv.mapIterator()
				if it == nil {
					return
				}
				var idx int
				for it.next() {
					tmp := it.key()
					if key := s.directBucketObject(Address(tmp.Addr), resolveTypedef(tmp.RealType)); key != nil {
						if !isPrimitiveType(key.RealType) {
							s.fillRefs(key)
							x.Children = append(x.Children, key)
							key.Name = fmt.Sprintf("key%d", idx)
						}
						x.allSize += key.allSize
						x.allCount += key.allCount
					}
					if it.values.fieldType.Size() > 0 {
						tmp = it.value()
					} else {
						tmp = xv.newVariable("", it.values.Addr, it.values.fieldType, s.mem)
					}
					if val := s.directBucketObject(Address(tmp.Addr), resolveTypedef(tmp.RealType)); val != nil {
						if !isPrimitiveType(val.RealType) {
							s.fillRefs(val)
							x.Children = append(x.Children, val)
							val.Name = fmt.Sprintf("val%d", idx)
						}
						x.allSize += val.allSize
						x.allCount += val.allCount
					}
					idx++
				}
			}
		}
	case *godwarf.StringType:
		strAddr, strLen, _ := readStringInfo(s.mem, s.bi.Arch, x.Addr, typ)
		if strLen > 0 {
			if y := s.findObject(Address(strAddr), fakeArrayType(uint64(strLen), &godwarf.UintType{BasicType: godwarf.BasicType{CommonType: godwarf.CommonType{ByteSize: 1, Name: "byte", ReflectKind: reflect.Uint8}, BitSize: 8, BitOffset: 0}})); y != nil {
				x.allSize += y.allSize
				x.allCount += y.allCount
			}
		}
	case *godwarf.SliceType:
		base, _ := readUintRaw(s.mem, x.Addr, int64(s.bi.Arch.PtrSize()))
		if base != 0 {
			cap_, _ := readUintRaw(s.mem, uint64(Address(x.Addr).Add(int64(s.bi.Arch.PtrSize())*2)), int64(s.bi.Arch.PtrSize()))
			if y := s.findObject(Address(base), fakeArrayType(cap_, typ.ElemType)); y != nil {
				s.fillRefs(y)
				x.Children = y.Children
				x.allSize += y.allSize
				x.allCount += y.allCount
			}
		}
	case *godwarf.InterfaceType:
		xv := newVariable("", x.Addr, x.RealType, s.bi, s.mem)
		_type, data, isnil := xv.readInterface()
		if isnil || data == nil {
			return
		}
		rtyp, _, err := runtimeTypeToDIE(_type, data.Addr)
		if err != nil {
			return
		}
		realtyp := resolveTypedef(rtyp)
		if ptrType, isPtr := realtyp.(*godwarf.PtrType); isPtr {
			ptrval, _ := readUintRaw(s.mem, data.Addr, int64(s.bi.Arch.PtrSize()))
			if ptrval != 0 {
				if y := s.findObject(Address(ptrval), resolveTypedef(ptrType)); y != nil {
					s.fillRefs(y)
					x.Children = y.Children
					x.allSize += y.allSize
					x.allCount += y.allCount
				}
			}
		}
	case *godwarf.StructType:
		// cache mem
		for _, field := range typ.Field {
			if isPrimitiveType(field.Type) {
				continue
			}
			y := &ReferenceVariable{
				Addr:     uint64(Address(x.Addr).Add(field.ByteOffset)),
				Name:     field.Name,
				RealType: resolveTypedef(field.Type),
			}
			s.fillRefs(y)
			x.Children = append(x.Children, y)
			x.allSize += y.allSize
			x.allCount += y.allCount
		}
	case *godwarf.ArrayType:
		eType := resolveTypedef(typ.Type)
		if isPrimitiveType(eType) {
			return
		}
		for i := int64(0); i < typ.Count; i++ {
			y := &ReferenceVariable{
				Addr:     uint64(Address(x.Addr).Add(i * eType.Size())),
				Name:     fmt.Sprintf("[%d]", i),
				RealType: eType,
			}
			s.fillRefs(y)
			x.Children = append(x.Children, y)
			x.allSize += y.allSize
			x.allCount += y.allCount
		}
	case *godwarf.FuncType:
		closureAddr, _ := readUintRaw(s.mem, x.Addr, int64(s.bi.Arch.PtrSize()))
		if closureAddr != 0 {
			funcAddr, _ := readUintRaw(s.mem, closureAddr, int64(s.bi.Arch.PtrSize()))
			if funcAddr != 0 {
				fn := s.bi.PCToFunc(funcAddr)
				if fn == nil {
					return
				}
				cst := fn.closureStructType(s.bi)
				if closure := s.findObject(Address(closureAddr), cst); closure != nil {
					s.fillRefs(closure)
					x.Children = closure.Children
					x.allSize += closure.allSize
					x.allCount += closure.allCount
				}
			}
		}
	default:
	}
}

func isPrimitiveType(typ godwarf.Type) bool {
	typ = resolveTypedef(typ)
	switch typ.(type) {
	case *godwarf.BoolType, *godwarf.FloatType, *godwarf.UintType,
		*godwarf.UcharType, *godwarf.CharType, *godwarf.IntType, *godwarf.ComplexType:
		return true
	}
	return false
}

func (t *Target) ObjectReference() ([]*ReferenceVariable, error) {
	scope, err := ThreadScope(t, t.CurrentThread())
	if err != nil {
		return nil, err
	}
	var allVariables []*ReferenceVariable

	heapScope := &HeapScope{mem: t.Memory(), bi: t.BinInfo()}
	heapScope.readHeap(scope)

	grs, _, _ := GoroutinesInfo(t, 0, 0)
	for _, gr := range grs {
		sf, _ := GoroutineStacktrace(t, gr, 512, 0)
		if len(sf) > 0 {
			ors := &ObjRefScope{gr: gr, HeapScope: heapScope, stackVisited: make(map[Address]bool)}
			for i := range sf {
				scope, _ := ConvertEvalScope(t, gr.ID, i, 0)
				locals, _ := scope.LocalVariables(loadSingleValue)
				for _, l := range locals {
					if l.Addr != 0 {
						root := &ReferenceVariable{
							Addr:     l.Addr,
							Name:     l.Name,
							RealType: l.RealType,
						}
						ors.fillRefs(root)
						allVariables = append(allVariables, root)
					}
				}
			}
		}
	}

	ors := &ObjRefScope{HeapScope: heapScope}
	pvs, _ := scope.PackageVariables(loadSingleValue)
	for _, pv := range pvs {
		if pv.Addr != 0 {
			root := &ReferenceVariable{
				Addr:     pv.Addr,
				Name:     pv.Name,
				RealType: pv.RealType,
			}
			ors.fillRefs(root)
			allVariables = append(allVariables, root)
		}
	}

	// Finalizers
	for _, r := range heapScope.specials {
		for _, child := range r.Children {
			if child.Addr != 0 {
				root := &ReferenceVariable{
					Addr:     child.Addr,
					Name:     child.Name,
					RealType: child.RealType,
				}
				ors.fillRefs(root)
				allVariables = append(allVariables, root)
			}
		}
	}
	return allVariables, nil
}
