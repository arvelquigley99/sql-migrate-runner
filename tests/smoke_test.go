package tests

import "testing"

func TestSmoke(t *testing.T) {
	if 1 != 1 {
		t.Fail()
	}
}
