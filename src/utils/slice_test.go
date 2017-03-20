package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSliceUnique(t *testing.T) {
	s := []string{"1", "2", "3"}

	assert.True(t, SliceUnique(s))
}

func TestSliceNotUnique(t *testing.T) {
	s := []string{"1", "2", "3", "1"}

	assert.False(t, SliceUnique(s))
}

func TestSliceContains(t *testing.T) {
	s := []string{"1", "2", "3"}
	assert.True(t, SliceContains(s, "1"))
}

func TestSliceNotContains(t *testing.T) {
	s := []string{"1", "2", "3"}
	assert.False(t, SliceContains(s, "4"))
}
