package main

import "testing"

func TestResolveDest(t *testing.T) {
	cases := []struct {
		name                       string
		dest, flagNS, flagProj     string
		defNS, defProj             string
		wantNS, wantProj, wantPath string
		wantErr                    bool
	}{
		{
			name: "relative with flags",
			dest: "notes/foo.md", flagNS: "nsA", flagProj: "projA",
			wantNS: "nsA", wantProj: "projA", wantPath: "notes/foo.md",
		},
		{
			name: "relative with config defaults",
			dest: "foo.md", defNS: "nsD", defProj: "projD",
			wantNS: "nsD", wantProj: "projD", wantPath: "foo.md",
		},
		{
			name: "relative flags override defaults",
			dest: "foo.md", flagNS: "nsF", flagProj: "projF", defNS: "nsD", defProj: "projD",
			wantNS: "nsF", wantProj: "projF", wantPath: "foo.md",
		},
		{
			name: "relative no flags no defaults is error",
			dest: "foo.md", wantErr: true,
		},
		{
			name: "relative project missing is error",
			dest: "foo.md", flagNS: "nsA", wantErr: true,
		},
		{
			name:   "absolute splits ns project path",
			dest:   "/nsB/projB/dir/foo.md",
			wantNS: "nsB", wantProj: "projB", wantPath: "dir/foo.md",
		},
		{
			name:   "absolute single-segment path",
			dest:   "/nsB/projB/foo.md",
			wantNS: "nsB", wantProj: "projB", wantPath: "foo.md",
		},
		{
			name: "absolute with matching flags ok",
			dest: "/nsB/projB/foo.md", flagNS: "nsB", flagProj: "projB",
			wantNS: "nsB", wantProj: "projB", wantPath: "foo.md",
		},
		{
			name: "absolute with conflicting namespace flag is error",
			dest: "/nsB/projB/foo.md", flagNS: "nsC", wantErr: true,
		},
		{
			name: "absolute with conflicting project flag is error",
			dest: "/nsB/projB/foo.md", flagProj: "projC", wantErr: true,
		},
		{
			name: "absolute too few segments is error",
			dest: "/nsB/projB", wantErr: true,
		},
		{
			name: "absolute empty path segment is error",
			dest: "/nsB/projB/", wantErr: true,
		},
		{
			name: "absolute ns-project shaped path resolves positionally",
			// The B-47 counter-example: a path that itself looks like ns/project.
			dest:   "/nsA/projA/nsB/projB/foo.md",
			wantNS: "nsA", wantProj: "projA", wantPath: "nsB/projB/foo.md",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, proj, path, err := resolveDest(tc.dest, tc.flagNS, tc.flagProj, tc.defNS, tc.defProj)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got ns=%q proj=%q path=%q", ns, proj, path)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ns != tc.wantNS || proj != tc.wantProj || path != tc.wantPath {
				t.Fatalf("got ns=%q proj=%q path=%q; want ns=%q proj=%q path=%q",
					ns, proj, path, tc.wantNS, tc.wantProj, tc.wantPath)
			}
		})
	}
}
