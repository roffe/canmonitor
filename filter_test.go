package main

import "testing"

func TestFilter(t *testing.T) {
	if !(*filterSet)(nil).pass(0x123) {
		t.Fatal("nil filter must pass everything")
	}
	inc, err := parseIDs("7E8, 0x123 1ff")
	if err != nil || len(inc) != 3 || !inc[0x1FF] {
		t.Fatalf("parseIDs: %v %v", inc, err)
	}
	if _, err := parseIDs("xyz"); err == nil {
		t.Fatal("want error on bad token")
	}
	fs := &filterSet{include: inc, exclude: map[uint32]bool{0x123: true}}
	if fs.pass(0x123) || !fs.pass(0x7E8) || fs.pass(0x666) {
		t.Fatal("include/exclude logic broken")
	}
}
