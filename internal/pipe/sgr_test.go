package pipe

import "testing"

func TestSGRApply(t *testing.T) {
	tests := []struct {
		seqs []string // parameter strings applied in order
		want string   // rendered parameters
	}{
		{[]string{""}, ""},
		{[]string{"0"}, ""},
		{[]string{"1;2;3;4;5;6;7;8;9;53"}, "1;2;3;5;6;7;8;9;53;4"},
		{[]string{"1;2", "22"}, ""},
		{[]string{"3", "23", "4", "24", "5;6", "25", "7", "27", "8", "28", "9", "29", "53", "55"}, ""},
		{[]string{"21"}, "4:2"},
		{[]string{"4:0"}, ""},
		{[]string{"4:5"}, "4:5"},
		{[]string{"4:9"}, ""},
		{[]string{"31;42"}, "31;42"},
		{[]string{"91;102"}, "91;102"},
		{[]string{"31", "39"}, ""},
		{[]string{"42", "49"}, ""},
		{[]string{"38;5;196;48;5;0"}, "38;5;196;48;5;0"},
		{[]string{"38;2;255;128;0"}, "38;2;255;128;0"},
		{[]string{"38:5:12"}, "38;5;12"},
		{[]string{"38:2:1:2:3"}, "38;2;1;2;3"},
		{[]string{"48:2::1:2:3"}, "48;2;1;2;3"},
		{[]string{"58;5;3"}, "58;5;3"},
		{[]string{"58:2::9:9:9", "59"}, ""},
		{[]string{"38;5;256"}, ""},           // out of range: ignored
		{[]string{"38;2;1;2"}, ""},           // too short
		{[]string{"38;7;1;31"}, "1;31"},      // unknown mode consumes its id only
		{[]string{"x;1;;31"}, "31"},          // malformed token ignored; empty token resets
		{[]string{"1;31", "0;32"}, "32"},     // reset inside a sequence
		{[]string{"1:2:3"}, "1"},             // sub-params of a plain code
		{[]string{"10;11;20;51;60"}, ""},     // fonts, frames, ideograms: not tracked
		{[]string{"123456789"}, ""},          // number too long
		{[]string{"38;5;", "1"}, "1;38;5;0"}, // empty index is 0
	}
	for _, tt := range tests {
		var s sgrState
		for _, p := range tt.seqs {
			s.apply([]byte(p))
		}
		if got := string(s.appendParams(nil)); got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.seqs, got, tt.want)
		}
	}
}

func TestSGRRoundTrip(t *testing.T) {
	for _, p := range []string{"1;31", "2;3;4:3;38;2;1;2;3;48;5;17;58;5;200", "7;9;53;97;107"} {
		var a, b sgrState
		a.apply([]byte(p))
		b.apply(a.appendParams(nil))
		if a != b {
			t.Errorf("%q does not round-trip: %+v vs %+v", p, a, b)
		}
	}
}

func TestSGRMaxLen(t *testing.T) {
	var s sgrState
	s.apply([]byte("1;2;3;4:5;5;6;7;8;9;53;38;2;255;255;255;48;2;255;255;255;58;2;255;255;255"))
	if n := len(appendTransition(nil, sgrState{attrs: attrBold}, s)) + len(sgrReset); n > maxSGRBytes {
		t.Fatalf("longest transition is %d bytes > maxSGRBytes", n)
	}
}
