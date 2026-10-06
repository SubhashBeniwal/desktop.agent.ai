package logbuf

import (
	"reflect"
	"testing"
)

func TestBufferSplitsAndBounds(t *testing.T) {
	b := New(2)
	var got []string
	b.OnLine(func(l string) { got = append(got, l) })

	b.Write([]byte("a\nb"))
	b.Write([]byte("c\r\nd\n"))

	if want := []string{"a", "bc", "d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("callback lines = %q, want %q", got, want)
	}
	if want := []string{"bc", "d"}; !reflect.DeepEqual(b.Lines(), want) {
		t.Fatalf("retained = %q, want %q", b.Lines(), want)
	}
}
