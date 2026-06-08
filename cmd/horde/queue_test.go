package main

import "testing"

func TestQueueCmdRegistered(t *testing.T) {
	app := newApp()
	var found bool
	for _, c := range app.Commands {
		if c.Name != "queue" {
			continue
		}
		found = true
		subs := map[string]bool{}
		for _, sc := range c.Commands {
			subs[sc.Name] = true
		}
		for _, want := range []string{"list", "prioritize", "cancel"} {
			if !subs[want] {
				t.Errorf("queue missing subcommand %q", want)
			}
		}
	}
	if !found {
		t.Error("queue command not registered")
	}
}
