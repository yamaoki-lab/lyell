package blobstore

import "testing"

func TestParseSum_RoundTripsWithString(t *testing.T) {
	want := sumOf("hello")

	got, err := ParseSum(want.String())
	if err != nil {
		t.Fatalf("ParseSum() error = %v, want nil", err)
	}
	if got != want {
		t.Errorf("ParseSum() = %s, want %s", got, want)
	}
}

func TestParseSum_RejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "空", in: ""},
		{name: "短い", in: "2cf24dba"},
		{name: "長い", in: sumOf("hello").String() + "00"},
		{name: "16進でない文字", in: "zz" + sumOf("hello").String()[2:]},
		{name: "パスの区切り", in: "../" + sumOf("hello").String()[3:]},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseSum(tt.in); err == nil {
				t.Errorf("ParseSum(%q) error = nil, want error", tt.in)
			}
		})
	}
}
