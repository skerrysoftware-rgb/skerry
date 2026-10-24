package agent

import "testing"

func TestPortAliasesMustLeadToAService(t *testing.T) {
	base := Config{Mode: ModeServe, WorkspacePath: "/Users/x/repo", Services: []string{"postgres"}, Secret: "0123456789abcdef0123456789abcdef", DataDisk: true}
	for _, tc := range []struct {
		alias PortAlias
		ok    bool
	}{
		{PortAlias{Port: 54320, To: 5432}, true},
		{PortAlias{Port: 54320, To: 22}, false},   // not a service's port
		{PortAlias{Port: 63790, To: 6379}, false}, // valkey isn't running
		{PortAlias{Port: 5432, To: 5432}, false},
		{PortAlias{Port: 0, To: 5432}, false},
	} {
		c := base
		c.PortAliases = []PortAlias{tc.alias}
		if err := c.Validate(); (err == nil) != tc.ok {
			t.Errorf("%+v: err = %v", tc.alias, err)
		}
	}
}
