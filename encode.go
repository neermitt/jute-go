package jute

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"sync"
)

func Marshal(v interface{}) ([]byte, error) {
	e := newEncodeState()

	err := e.marshal(v)
	if err != nil {
		return nil, err
	}
	buf := append([]byte(nil), e.Bytes()...)

	e.Reset()
	encodeStatePool.Put(e)

	return buf, nil
}

// Marshaler is the interface implemented by types that
// can marshal themselves into valid Jute.
type Marshaler interface {
	MarshalJute() ([]byte, error)
}

// An UnsupportedTypeError is returned by Marshal when attempting
// to encode an unsupported value type.
type UnsupportedTypeError struct {
	Type reflect.Type
}

func (e *UnsupportedTypeError) Error() string {
	return "jute: unsupported type: " + e.Type.String()
}

type MarshalerError struct {
	Type reflect.Type
	Err  error
}

func (e *MarshalerError) Error() string {
	return "jute: error calling MarshalJute for type " + e.Type.String() + ": " + e.Err.Error()
}

// An encodeState encodes JSON into a bytes.Buffer.
type encodeState struct {
	bytes.Buffer // accumulated output
}

var encodeStatePool sync.Pool

func newEncodeState() *encodeState {
	if v := encodeStatePool.Get(); v != nil {
		e := v.(*encodeState)
		e.Reset()
		return e
	}
	return new(encodeState)
}

// juteError is an error wrapper type for internal use only.
// Panics with errors are wrapped in juteError so that the top-level recover
// can distinguish intentional panics from this package.
type juteError struct{ error }

func (e *encodeState) marshal(v interface{}) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if je, ok := r.(juteError); ok {
				err = je.error
			} else {
				panic(r)
			}
		}
	}()
	e.reflectValue(reflect.ValueOf(v))
	return nil
}

// error aborts the encoding by panicking with err wrapped in jsonError.
func (e *encodeState) error(err error) {
	panic(juteError{err})
}

func (e *encodeState) reflectValue(v reflect.Value) {
	valueEncoder(v)(e, v)
}

type encoderFunc func(e *encodeState, v reflect.Value)

var encoderCache sync.Map // map[reflect.Type]encoderFunc

func valueEncoder(v reflect.Value) encoderFunc {
	return typeEncoder(v.Type())
}

func typeEncoder(t reflect.Type) encoderFunc {
	if fi, ok := encoderCache.Load(t); ok {
		return fi.(encoderFunc)
	}

	// To deal with recursive types, populate the map with an
	// indirect func before we build it. This type waits on the
	// real func (f) to be ready and then calls it. This indirect
	// func is only used for recursive types.
	var (
		wg sync.WaitGroup
		f  encoderFunc
	)
	wg.Add(1)
	fi, loaded := encoderCache.LoadOrStore(t, encoderFunc(func(e *encodeState, v reflect.Value) {
		wg.Wait()
		f(e, v)
	}))
	if loaded {
		return fi.(encoderFunc)
	}

	// Compute the real encoder and replace the indirect func with it.
	f = newTypeEncoder(t)
	wg.Done()
	encoderCache.Store(t, f)
	return f
}

var marshalerType = reflect.TypeOf(new(Marshaler)).Elem()

// newTypeEncoder constructs an encoderFunc for a type.
func newTypeEncoder(t reflect.Type) encoderFunc {
	if t.Implements(marshalerType) {
		return marshalerEncoder
	}

	switch t.Kind() {
	case reflect.Bool:
		return boolEncoder
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return int32Encoder
	case reflect.Uint64, reflect.Int64:
		return int64Encoder
	case reflect.String:
		return stringEncoder
	case reflect.Interface:
		return interfaceEncoder
	case reflect.Struct:
		return newStructEncoder(t)
	case reflect.Slice:
		return newSliceEncoder(t)
	case reflect.Array:
		return newArrayEncoder(t)
	case reflect.Ptr:
		return newPtrEncoder(t)
	default:
		return unsupportedTypeEncoder
	}
}

func boolEncoder(e *encodeState, v reflect.Value) {
	binary.Write(e, binary.BigEndian, v.Bool())
}

func int32Encoder(e *encodeState, v reflect.Value) {
	binary.Write(e, binary.BigEndian, uint32(v.Int()))
}

func int64Encoder(e *encodeState, v reflect.Value) {
	binary.Write(e, binary.BigEndian, v.Int())
}

func stringEncoder(e *encodeState, v reflect.Value) {
	str := v.String()
	binary.Write(e, binary.BigEndian, int32(len(str)))
	e.WriteString(str)
}

func encodeByteSlice(e *encodeState, v reflect.Value) {
	if v.IsNil() {
		binary.Write(e, binary.BigEndian, uint32(0xffffffff))
		return
	}
	s := v.Bytes()
	binary.Write(e, binary.BigEndian, uint32(len(s)))
	e.Write(s)
}

// sliceEncoder just wraps an arrayEncoder, checking to make sure the value isn't nil.
type sliceEncoder struct {
	arrayEnc encoderFunc
}

func (se *sliceEncoder) encode(e *encodeState, v reflect.Value) {
	if v.IsNil() {
		binary.Write(e, binary.BigEndian, uint32(0xffffffff))
		return
	}
	se.arrayEnc(e, v)
}

func newSliceEncoder(t reflect.Type) encoderFunc {
	// Byte slices get special treatment; arrays don't.
	if t.Elem().Kind() == reflect.Uint8 {
		p := reflect.PtrTo(t.Elem())
		if !p.Implements(marshalerType) {
			return encodeByteSlice
		}
	}
	enc := &sliceEncoder{newArrayEncoder(t)}
	return enc.encode
}

type arrayEncoder struct {
	elemEnc encoderFunc
}

func (ae *arrayEncoder) encode(e *encodeState, v reflect.Value) {
	count := v.Len()
	binary.Write(e, binary.BigEndian, uint32(count))
	for i := 0; i < count; i++ {
		ae.elemEnc(e, v.Index(i))
	}
}

func newArrayEncoder(t reflect.Type) encoderFunc {
	enc := &arrayEncoder{typeEncoder(t.Elem())}
	return enc.encode
}

type ptrEncoder struct {
	elemEnc encoderFunc
}

func (pe *ptrEncoder) encode(e *encodeState, v reflect.Value) {
	if v.IsNil() {
		return
	}
	pe.elemEnc(e, v.Elem())
}

func newPtrEncoder(t reflect.Type) encoderFunc {
	enc := &ptrEncoder{typeEncoder(t.Elem())}
	return enc.encode
}

type structEncoder struct {
	fields    []field
	fieldEncs []encoderFunc
}

func (se *structEncoder) encode(e *encodeState, v reflect.Value) {
	for i, f := range se.fields {
		fv := fieldByIndex(v, f.index)
		se.fieldEncs[i](e, fv)
	}
}

func newStructEncoder(t reflect.Type) encoderFunc {
	fields := cachedTypeFields(t)
	se := &structEncoder{
		fields:    fields,
		fieldEncs: make([]encoderFunc, len(fields)),
	}
	for i, f := range fields {
		se.fieldEncs[i] = typeEncoder(typeByIndex(t, f.index))
	}
	return se.encode
}

func marshalerEncoder(e *encodeState, v reflect.Value) {
	if v.Kind() == reflect.Ptr && v.IsNil() {
		return
	}
	m, ok := v.Interface().(Marshaler)
	if !ok {
		return
	}
	b, err := m.MarshalJute()
	if err == nil {
		// copy b into buffer, checking validity.
		_, err = e.Buffer.Write(b)
	}
	if err != nil {
		e.error(&MarshalerError{v.Type(), err})
	}
}

func interfaceEncoder(e *encodeState, v reflect.Value) {
	if v.IsNil() {
		return
	}
	e.reflectValue(v.Elem())
}

func unsupportedTypeEncoder(e *encodeState, v reflect.Value) {
	e.error(&UnsupportedTypeError{v.Type()})
}
