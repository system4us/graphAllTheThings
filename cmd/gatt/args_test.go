package main

import (
	"reflect"
	"testing"
)

func TestHoistPositionals(t *testing.T) {
	cases := []struct {
		args []string
		n    int
		want []string
	}{
		{[]string{"a", "b", "--graph", "g.db"}, 2, []string{"a", "b", "--graph", "g.db"}},
		{[]string{"--graph", "g.db", "a", "b"}, 2, []string{"a", "b", "--graph", "g.db"}},
		{[]string{"--graph=g.db", "a", "--depth", "3", "b"}, 2, []string{"a", "b", "--graph=g.db", "--depth", "3"}},
		{[]string{"--regex", "foo", "--limit", "0"}, 1, []string{"foo", "--regex", "--limit", "0"}},
		{[]string{"--depth", "2"}, 1, []string{"--depth", "2"}},
		{[]string{"node", "key=value", "--clear"}, 1, []string{"node", "key=value", "--clear"}},
	}
	for _, c := range cases {
		if got := hoistPositionals(c.args, c.n); !reflect.DeepEqual(got, c.want) {
			t.Errorf("hoistPositionals(%q, %d) = %q, want %q", c.args, c.n, got, c.want)
		}
	}
}
