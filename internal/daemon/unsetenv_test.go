//go:build linux || darwin

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A client has to be able to take a variable away from a window's
// program, not only add one: the daemon's own environment carries things an
// agent must not see — an API key, CLAUDECODE marking it as running under an
// agent already — and env can only set. WindowOpts.UnsetEnv could do it from
// the start; the route could not reach it.
//
// The control case is what makes this more than an assertion that the child
// prints "absent": without unsetEnv both variables have to be there, or
// "absent" would also be what a daemon that ignored env entirely produces.
func TestCreateWindowUnsetEnv(t *testing.T) {
	t.Setenv("SHELLD_TEST_INHERITED", "yes")
	sock := boot(t)
	if st, _, raw := request(t, sock, "POST", "/sessions", `{"name":"s"}`); st != 200 {
		t.Fatalf("create session: %d %s", st, raw)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "env.sh")
	body := "#!/bin/sh\n" +
		`echo "A=${SHELLD_A-absent} INH=${SHELLD_TEST_INHERITED-absent} END"` + "\n" +
		"exec sleep 600\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(name, extra string) string {
		t.Helper()
		st, _, raw := request(t, sock, "POST", "/sessions/s/windows",
			`{"name":"`+name+`","cols":120,"rows":24,"shell":"`+p+`","env":["SHELLD_A=1"]`+extra+`}`)
		if st != 200 {
			t.Fatalf("create %s: %d %s", name, st, raw)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, v, raw := request(t, sock, "GET", "/sessions/s/windows/"+name+"/capture?lines=10", "")
			for _, l := range strings.Split(v["output"].(string), "\n") {
				if l = strings.TrimSpace(l); strings.HasSuffix(l, "END") {
					return l
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("window %s never printed its environment: %s", name, raw)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	if got, want := line("keep", ""), "A=1 INH=yes END"; got != want {
		t.Fatalf("without unsetEnv: %q, want %q — the premise of the case below", got, want)
	}
	// A name in both env and unsetEnv ends up unset, as WindowOpts documents:
	// unset is applied last.
	if got, want := line("drop", `,"unsetEnv":["SHELLD_TEST_INHERITED","SHELLD_A"]`), "A=absent INH=absent END"; got != want {
		t.Errorf("with unsetEnv: %q, want %q", got, want)
	}

	// unsetEnv is strict where env is loose, because a name silently dropped
	// here leaves a variable set that the caller asked to have removed. serde
	// rejects each of these, including the null element, which encoding/json
	// would otherwise decode into an empty name.
	for i, bad := range []string{
		`"unsetEnv":"SHELLD_A"`,
		`"unsetEnv":[1]`,
		`"unsetEnv":[null]`,
		`"unsetEnv":{"SHELLD_A":true}`,
		`"unsetEnv":["ok",2]`,
	} {
		name := "bad" + string(rune('a'+i))
		st, v, raw := request(t, sock, "POST", "/sessions/s/windows", `{"name":"`+name+`",`+bad+`}`)
		if st != 400 {
			t.Errorf("%s: status %d want 400: %s", bad, st, raw)
			continue
		}
		if v["error"] != "invalid unsetEnv" {
			t.Errorf("%s: error %q want %q", bad, v["error"], "invalid unsetEnv")
		}
	}
	// Absent, null and empty are all "nothing to unset", as an Option<Vec> with
	// serde's default is.
	for name, field := range map[string]string{
		"null":   `,"unsetEnv":null`,
		"empty":  `,"unsetEnv":[]`,
		"absent": ``,
	} {
		st, _, raw := request(t, sock, "POST", "/sessions/s/windows",
			`{"name":"ok-`+name+`","shell":"`+p+`"`+field+`}`)
		if st != 200 {
			t.Errorf("unsetEnv %s: status %d want 200: %s", name, st, raw)
		}
	}
}
