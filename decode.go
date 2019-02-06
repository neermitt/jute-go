package jute

import (
	"bytes"
	"encoding/binary"
	"io"
	"reflect"
	"sync"
)

func Unmarshal(data []byte, v interface{}) error {
	// Check for well-formedness.
	// Avoids filling out half a data structure
	// before discovering a JSON syntax error.
	d := newDecodeState(data)
	return d.Unmarshal(v)
}

// Unmarshaler is the interface implemented by types
// that can unmarshal a JSON description of themselves.
// The input can be assumed to be a valid encoding of
// a JSON value. UnmarshalJSON must copy the JSON data
// if it wishes to retain the data after returning.
//
// By convention, to approximate the behavior of Unmarshal itself,
// Unmarshalers implement UnmarshalJute(bytes.Reader) as a no-op.
type Unmarshaler interface {
	UnmarshalJute(ds *DecodeState) error
}

// An UnmarshalTypeError describes a JSON value that was
// not appropriate for a value of a specific Go type.
type UnmarshalTypeError struct {
	Value  string       // description of JSON value - "bool", "array", "number -5"
	Type   reflect.Type // type of Go value it could not be assigned to
	Offset int64        // error occurred after reading Offset bytes
	Struct string       // name of the struct type containing the field
	Field  string       // name of the field holding the Go value
}

func (e *UnmarshalTypeError) Error() string {
	if e.Struct != "" || e.Field != "" {
		return "json: cannot unmarshal " + e.Value + " into Go struct field " + e.Struct + "." + e.Field + " of type " + e.Type.String()
	}
	return "json: cannot unmarshal " + e.Value + " into Go value of type " + e.Type.String()
}

// An InvalidUnmarshalError describes an invalid argument passed to Unmarshal.
// (The argument to Unmarshal must be a non-nil pointer.)
type InvalidUnmarshalError struct {
	Type reflect.Type
}

func (e *InvalidUnmarshalError) Error() string {
	if e.Type == nil {
		return "json: Unmarshal(nil)"
	}

	if e.Type.Kind() != reflect.Ptr {
		return "json: Unmarshal(non-pointer " + e.Type.String() + ")"
	}
	return "json: Unmarshal(nil " + e.Type.String() + ")"
}

// DecodeState represents the state while decoding a JSON value.
type DecodeState struct {
	*bytes.Reader
}

func newDecodeState(data []byte) *DecodeState {
	return &DecodeState{bytes.NewReader(data)}
}

func (d *DecodeState) Unmarshal(v interface{}) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if je, ok := r.(juteError); ok {
				err = je.error
			} else {
				panic(r)
			}
		}
	}()
	d.reflectValue(reflect.ValueOf(v))
	return nil
}

// error aborts the encoding by panicking with err wrapped in jsonError.
func (d *DecodeState) error(err error) {
	panic(juteError{err})
}

func (d *DecodeState) reflectValue(v reflect.Value) {
	valueDecoder(v)(d, v)
}

type decoderFunc func(d *DecodeState, v reflect.Value)

var decoderCache sync.Map // map[reflect.Type]decoderFunc

func valueDecoder(v reflect.Value) decoderFunc {
	return typeDecoder(v.Type())
}

func typeDecoder(t reflect.Type) decoderFunc {
	if fi, ok := decoderCache.Load(t); ok {
		return fi.(decoderFunc)
	}

	// To deal with recursive types, populate the map with an
	// indirect func before we build it. This type waits on the
	// real func (f) to be ready and then calls it. This indirect
	// func is only used for recursive types.
	var (
		wg sync.WaitGroup
		f  decoderFunc
	)
	wg.Add(1)
	fi, loaded := decoderCache.LoadOrStore(t, decoderFunc(func(e *DecodeState, v reflect.Value) {
		wg.Wait()
		f(e, v)
	}))
	if loaded {
		return fi.(decoderFunc)
	}

	// Compute the real encoder and replace the indirect func with it.
	f = newTypeDecoder(t)
	wg.Done()
	decoderCache.Store(t, f)
	return f
}

var unmarshalerType = reflect.TypeOf(new(Unmarshaler)).Elem()

// newTypeEncoder constructs an encoderFunc for a type.
func newTypeDecoder(t reflect.Type) decoderFunc {
	if t.Implements(unmarshalerType) {
		return unmarshalerDecoder
	}

	switch t.Kind() {
	case reflect.Bool:
		return boolDecoder
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return int32Decoder
	case reflect.Uint64, reflect.Int64:
		return int64Decoder
	case reflect.String:
		return stringDecoder
	case reflect.Struct:
		return newStructDecoder(t)
	case reflect.Slice:
		return newSliceDecoder(t)
	case reflect.Array:
		return newArrayDecoder(t)
	case reflect.Ptr:
		return newPtrDecoder(t)
	default:
		return unsupportedTypeDecoder
	}
}

func boolDecoder(e *DecodeState, v reflect.Value) {
	var b bool
	binary.Read(e, binary.BigEndian, &b)
	v.SetBool(b)
}

func int32Decoder(e *DecodeState, v reflect.Value) {
	var n uint32
	binary.Read(e, binary.BigEndian, &n)
	v.SetInt(int64(n))
}

func int64Decoder(e *DecodeState, v reflect.Value) {
	var n uint64
	binary.Read(e, binary.BigEndian, &n)
	v.SetInt(int64(n))
}

func stringDecoder(e *DecodeState, v reflect.Value) {
	var n uint32
	binary.Read(e, binary.BigEndian, &n)
	if n > 0 {
		bs := make([]byte, n)
		io.ReadFull(e, bs)
		v.SetString(string(bs))
	} else {
		v.SetString("")
	}
}

func decodeByteSlice(e *DecodeState, v reflect.Value) {
	var n uint32
	binary.Read(e, binary.BigEndian, &n)

	count := int32(n)
	if count < 0 {
		v.SetBytes(nil)
	} else {
		bs := make([]byte, count)
		io.ReadFull(e, bs)
		v.SetBytes(bs)
	}
}

// sliceDecoder just wraps an arrayDecoder, checking to make sure the value isn't nil.
type sliceDecoder struct {
	arrayDec decoderFunc
	elemType reflect.Type
}

func (sd *sliceDecoder) decode(e *DecodeState, v reflect.Value) {
	var n uint32
	binary.Read(e, binary.BigEndian, &n)
	if int(n) < 0 {
		v.Set(reflect.Zero(reflect.SliceOf(sd.elemType)))
		return
	} else {
		count := int(n)
		v.Set(reflect.MakeSlice(reflect.SliceOf(sd.elemType), count, count))
	}
	sd.arrayDec(e, v)
}

func newSliceDecoder(t reflect.Type) decoderFunc {
	// Byte slices get special treatment; arrays don't.
	if t.Elem().Kind() == reflect.Uint8 {
		p := reflect.PtrTo(t.Elem())
		if !p.Implements(marshalerType) {
			return decodeByteSlice
		}
	}
	enc := &sliceDecoder{newArrayDecoder(t), t.Elem()}
	return enc.decode
}

type arrayDecoder struct {
	elemDec decoderFunc
}

func (ad *arrayDecoder) decode(e *DecodeState, v reflect.Value) {
	count := v.Len()
	for i := 0; i < count; i++ {
		ad.elemDec(e, v.Index(i))
	}
}

func newArrayDecoder(t reflect.Type) decoderFunc {
	enc := &arrayDecoder{typeDecoder(t.Elem())}
	return enc.decode
}

type ptrDecoder struct {
	elemDec decoderFunc
}

func (pd *ptrDecoder) decode(e *DecodeState, v reflect.Value) {
	if v.IsNil() {
		v.Set(reflect.New(v.Type().Elem()))
	}
	pd.elemDec(e, v.Elem())
}

func newPtrDecoder(t reflect.Type) decoderFunc {
	enc := &ptrDecoder{typeDecoder(t.Elem())}
	return enc.decode
}

type structDecoder struct {
	fields    []field
	fieldDecs []decoderFunc
}

func (sd *structDecoder) decode(e *DecodeState, v reflect.Value) {
	for i, f := range sd.fields {
		fv := fieldByIndex(v, f.index)
		sd.fieldDecs[i](e, fv)
	}
}

func newStructDecoder(t reflect.Type) decoderFunc {
	fields := cachedTypeFields(t)
	se := &structDecoder{
		fields:    fields,
		fieldDecs: make([]decoderFunc, len(fields)),
	}
	for i, f := range fields {
		se.fieldDecs[i] = typeDecoder(typeByIndex(t, f.index))
	}
	return se.decode
}

func unmarshalerDecoder(e *DecodeState, v reflect.Value) {
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
	}
	m, ok := v.Interface().(Unmarshaler)
	if !ok {
		return
	}
	err := m.UnmarshalJute(e)
	if err != nil {
		e.error(&MarshalerError{v.Type(), err})
	}
}

func unsupportedTypeDecoder(e *DecodeState, v reflect.Value) {
	e.error(&UnsupportedTypeError{v.Type()})
}
