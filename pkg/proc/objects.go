package proc

import (
	"fmt"
	"reflect"

	"github.com/go-delve/delve/pkg/dwarf/godwarf"
	"github.com/go-delve/delve/pkg/logflags"
)

type ReferenceVariable struct {
	BriefVariable

	Children []*ReferenceVariable

	// all referenced object size
	allSize int64
	// all referenced object count
	allCount int64
}

type ObjRefScope struct {
	*HeapScope

	gr *G
}

// todo:
// 1. mark allSize and allCount
// 2. 去重
// 3. 尽量识别所有能识别的类型
// real type
func (s *ObjRefScope) findObject(addr Address, typ godwarf.Type) (v *ReferenceVariable) {
	h := s.findHeapInfo(addr)
	if h == nil {
		// Not in Go heap
		if s.gr == nil || uint64(addr) < s.gr.stack.lo || uint64(addr) > s.gr.stack.hi {
			// Not in Go stack
			return nil
		}
		v = &ReferenceVariable{
			BriefVariable: BriefVariable{
				Addr:     uint64(addr),
				RealType: resolveTypedef(typ),
			},
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
		BriefVariable: BriefVariable{
			Addr:     uint64(addr),
			RealType: resolveTypedef(typ),
		},
	}
	v.allSize += h.size
	// mark each unknown type pointer
	for i := int64(0); i < h.size; i += int64(s.bi.Arch.PtrSize()) {
		a := x.Add(i)
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

func (s *ObjRefScope) fillRefs(x *ReferenceVariable) {
	switch typ := x.RealType.(type) {
	case *godwarf.PtrType:
		ptrval, _ := readUintRaw(s.mem, x.Addr, int64(s.bi.Arch.PtrSize()))
		if ptrval != 0 {
			if y := s.findObject(Address(ptrval), resolveTypedef(typ.Type)); y != nil {
				s.fillRefs(y)
				x.Children = []*ReferenceVariable{y}
				y.Name = ".ptr"
				x.allSize += y.allSize
				x.allCount += y.allCount + 1
			}
		}
	case *godwarf.VoidType:
		return
	case *godwarf.ChanType:
		ptrval, _ := readUintRaw(s.mem, x.Addr, int64(s.bi.Arch.PtrSize()))
		if ptrval != 0 {
			if y := s.findObject(Address(ptrval), resolveTypedef(typ.Type)); y != nil {
				structType, ok := y.RealType.(*godwarf.StructType)
				if !ok {
					logflags.DebuggerLogger().Errorf("bad channel type %v", y.RealType.String())
					return
				}

				chanLen, _ := readUintRaw(s.mem, uint64(Address(x.Addr).Add(structType.Field[1].ByteOffset)), structType.Field[1].ByteSize)

				newStructType := &godwarf.StructType{}
				*newStructType = *structType
				newStructType.Field = make([]*godwarf.StructField, len(structType.Field))

				for i := range structType.Field {
					field := &godwarf.StructField{}
					*field = *structType.Field[i]
					if field.Name == "buf" {
						field.Type = pointerTo(fakeArrayType(chanLen, typ.ElemType), s.bi.Arch)
					}
					newStructType.Field[i] = field
				}
				y.RealType = newStructType
				s.fillRefs(y)
				x.Children = []*ReferenceVariable{y}
				y.Name = ".ptr"
				x.allSize += y.allSize
				x.allCount += y.allCount + 1
			}
		}
	case *godwarf.MapType:
		ptrval, _ := readUintRaw(s.mem, x.Addr, int64(s.bi.Arch.PtrSize()))
		if ptrval != 0 {
			if y := s.findObject(Address(ptrval), resolveTypedef(typ.Type)); y != nil {
				xv := newVariable("", x.Addr, x.RealType, s.bi, s.mem)
				it := xv.mapIterator()
				if it == nil {
					return
				}
				var idx int
				for it.next() {
					tmp := it.key()
					if key := s.findObject(Address(tmp.Addr), resolveTypedef(tmp.RealType)); key != nil {
						s.fillRefs(key)
						x.Children = append(x.Children, key)
						key.Name = fmt.Sprintf("key%d", idx)
						x.allSize += key.allSize
						x.allCount += key.allCount + 1
					}
					if it.values.fieldType.Size() > 0 {
						tmp = it.value()
					} else {
						tmp = xv.newVariable("", it.values.Addr, it.values.fieldType, s.mem)
					}
					if val := s.findObject(Address(tmp.Addr), resolveTypedef(tmp.RealType)); val != nil {
						s.fillRefs(val)
						x.Children = append(x.Children, val)
						val.Name = fmt.Sprintf("val%d", idx)
						x.allSize += val.allSize
						x.allCount += val.allCount + 1
					}
					idx++
				}
				x.allSize += y.allSize
				x.allCount += y.allCount + 1
			}
		}
	case *godwarf.StringType:
		strAddr, strLen, _ := readStringInfo(s.mem, s.bi.Arch, x.Addr, typ)
		if strLen > 0 {
			if y := s.findObject(Address(strAddr), fakeArrayType(uint64(strLen), &godwarf.UintType{BasicType: godwarf.BasicType{CommonType: godwarf.CommonType{ByteSize: 1, Name: "byte", ReflectKind: reflect.Uint8}, BitSize: 8, BitOffset: 0}})); y != nil {
				x.allSize += y.allSize
				x.allCount += y.allCount + 1
			}
		}
	case *godwarf.SliceType:
		base, _ := readUintRaw(s.mem, x.Addr, int64(s.bi.Arch.PtrSize()))
		if base != 0 {
			cap_, _ := readUintRaw(s.mem, uint64(Address(x.Addr).Add(int64(s.bi.Arch.PtrSize())*2)), int64(s.bi.Arch.PtrSize()))
			if y := s.findObject(Address(base), fakeArrayType(cap_, typ.ElemType)); y != nil {
				if !isPrimitiveType(typ.ElemType) {
					s.fillRefs(y)
				}
				x.Children = []*ReferenceVariable{y}
				y.Name = ".ptr"
				x.allSize += y.allSize
				x.allCount += y.allCount + 1
			}
		}
	case *godwarf.InterfaceType:
		xv := &Variable{BriefVariable: x.BriefVariable, mem: s.mem}
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
					x.Children = []*ReferenceVariable{y}
					y.Name = ".data"
					x.allSize += y.allSize
					x.allCount += y.allCount + 1
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
				BriefVariable: BriefVariable{
					Addr:     uint64(Address(x.Addr).Add(field.ByteOffset)),
					Name:     field.Name,
					RealType: resolveTypedef(field.Type),
				},
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
				BriefVariable: BriefVariable{
					Addr:     uint64(Address(x.Addr).Add(i * eType.Size())),
					Name:     fmt.Sprintf("[%d]", i),
					RealType: resolveTypedef(eType),
				},
			}
			s.fillRefs(y)
			x.Children = append(x.Children, y)
			x.allSize += y.allSize
			x.allCount += y.allCount
		}
	case *godwarf.FuncType:
		x.allSize += int64(s.bi.Arch.PtrSize())
		closureAddr, _ := readUintRaw(s.mem, x.Addr, int64(s.bi.Arch.PtrSize()))
		if closureAddr != 0 {
			if closure := s.findObject(Address(closureAddr), typ); closure != nil {
				funcAddr, _ := readUintRaw(s.mem, closureAddr, int64(s.bi.Arch.PtrSize()))
				if funcAddr != 0 {
					fn := s.bi.PCToFunc(funcAddr)
					if fn == nil {
						return
					}
					cst := fn.closureStructType(s.bi)
					closure.RealType = cst
					s.fillRefs(closure)
					x.Children = []*ReferenceVariable{closure}
					closure.Name = fn.Name
					x.allSize += closure.allSize
					x.allCount += closure.allCount + 1
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
			ors := &ObjRefScope{gr: gr, HeapScope: heapScope}
			for i := range sf {
				scope, _ := ConvertEvalScope(t, gr.ID, i, 0)
				locals, _ := scope.LocalVariables(loadSingleValue)
				for _, l := range locals {
					if l.Addr != 0 {
						root := &ReferenceVariable{
							BriefVariable: BriefVariable{
								Addr:     l.Addr,
								Name:     l.Name,
								RealType: l.RealType,
							},
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
				BriefVariable: BriefVariable{
					Addr:     pv.Addr,
					Name:     pv.Name,
					RealType: pv.RealType,
				},
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
					BriefVariable: BriefVariable{
						Addr:     child.Addr,
						Name:     child.Name,
						RealType: child.RealType,
					},
				}
				ors.fillRefs(root)
				allVariables = append(allVariables, root)
			}
		}
	}
	return allVariables, nil
}
