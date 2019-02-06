package jute_test

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/neermitt/jute-go"
)

func TestEncodeDecodePacket(t *testing.T) {
	t.Parallel()
	testSet(t)
}

func BenchmarkEncodeDecodePacket(b *testing.B) {
	for n := 0; n < b.N; n++ {
		testSet(b)
	}
}

func testSet(t testing.TB) {
	encodeDecodeTest(t, &boolTest{true})
	encodeDecodeTest(t, &boolTest{false})
	encodeDecodeTest(t, &intTest{-2, 5, 10})
	encodeDecodeTest(t, &stringTest{""})
	encodeDecodeTest(t, &stringTest{"I have value"})
	encodeDecodeTest(t, &byteArrayTest{1, nil})
	encodeDecodeTest(t, &byteArrayTest{1, []byte{4, 5, 6}})
	encodeDecodeTest(t, &stringArrayTest{[]string{"foo", "bar"}})
	encodeDecodeTest(t, &nestedTest{intTest{10, 5, 6}})
	encodeDecodeTest(t, &nestedTest{intTest{}})
	encodeDecodeTest(t, &nestedArrayTest{[]intTest{}})
	encodeDecodeTest(t, &nestedArrayTest{[]intTest{{2, 3, 4}, {5, 6, 7}}})
	encodeDecodeTest(t, &multiRequest{Ops: []multiRequestOp{{multiHeader{opClose, false, -1}, &closeRequest{"/", -1}}}})
}

func encodeDecodeTest(t testing.TB, r interface{}) {
	buf, err := jute.Marshal(r)
	if err != nil {
		t.Errorf("encodePacket returned non-nil error %+v\n", err)
		return
	}
	t.Logf("%+v %x", r, buf)
	r2 := reflect.New(reflect.ValueOf(r).Elem().Type()).Interface()
	err = jute.Unmarshal(buf, r2)
	if err != nil {
		t.Errorf("decodePacket returned non-nil error %+v\n", err)
		return
	}
	if !reflect.DeepEqual(r, r2) {
		t.Errorf("results don't match: %+v != %+v", r, r2)
		return
	}
}

type boolTest struct {
	Field1 bool
}

type intTest struct {
	Field1 int32
	Field2 int32
	Field3 int64
}

type stringTest struct {
	Field1 string
}

type byteArrayTest struct {
	Field1 int32
	Field2 []byte
}

type stringArrayTest struct {
	Field1 []string
}

type nestedTest struct {
	Inner intTest
}

type nestedArrayTest struct {
	Inner []intTest
}

type multiHeader struct {
	Type int32
	Done bool
	Err  int32
}

type multiRequestOp struct {
	Header multiHeader
	Op     interface{}
}
type multiRequest struct {
	Ops        []multiRequestOp
	DoneHeader multiHeader
}

func (r *multiRequest) MarshalJute() ([]byte, error) {
	buf := new(bytes.Buffer)
	for _, op := range r.Ops {
		op.Header.Done = false
		data, err := jute.Marshal(op)
		if err != nil {
			return nil, err
		}
		buf.Write(data)
	}
	r.DoneHeader.Done = true
	data, err := jute.Marshal(r.DoneHeader)
	if err != nil {
		return nil, err
	}
	buf.Write(data)

	return buf.Bytes(), nil
}

func (r *multiRequest) UnmarshalJute(ds *jute.DecodeState) error {
	r.Ops = make([]multiRequestOp, 0)
	r.DoneHeader = multiHeader{-1, true, -1}
	for {
		header := &multiHeader{}
		err := ds.Unmarshal(header)
		if err != nil {
			return err
		}
		if header.Done {
			r.DoneHeader = *header
			break
		}

		req := requestStructForOp(header.Type)
		if req == nil {
			return errors.New("bad request")
		}
		err = ds.Unmarshal(req)
		if err != nil {
			return err
		}
		r.Ops = append(r.Ops, multiRequestOp{*header, req})
	}
	return nil
}

const (
	opCreate = 1
	opClose  = 2
)

type closeRequest struct {
	Field1 string
	Field2 int32
}
type createRequest struct{}

func requestStructForOp(op int32) interface{} {
	switch op {
	case opClose:
		return &closeRequest{}
	case opCreate:
		return &createRequest{}
	}
	return nil
}
