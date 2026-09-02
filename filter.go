package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
)

// software RX filter, hot-swappable while connected
var filterPtr atomic.Pointer[filterSet]

type filterSet struct {
	include, exclude map[uint32]bool
}

func (fs *filterSet) pass(id uint32) bool {
	if fs == nil {
		return true
	}
	if fs.exclude[id] {
		return false
	}
	return len(fs.include) == 0 || fs.include[id]
}

// parseIDs parses whitespace/comma-separated hex IDs ("7E8, 0x123 1FF").
func parseIDs(s string) (map[uint32]bool, error) {
	m := map[uint32]bool{}
	for _, tok := range strings.Fields(strings.ReplaceAll(s, ",", " ")) {
		id, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimPrefix(tok, "0x"), "0X"), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("bad ID %q", tok)
		}
		m[uint32(id)] = true
	}
	return m, nil
}
