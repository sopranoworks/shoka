package ui

import (
	"reflect"
	"testing"

	"github.com/sopranoworks/shoka/pkg/authz"
	"github.com/sopranoworks/shoka/pkg/uiws"
)

// TestGateTables_MatchPreExtraction is the directive's behaviour-preserving proof for
// the 2026-06-21 ui-split: after the auth/user/OAuth core rows moved to
// uiws.CoreLevels and the gate became table-parameterized (Client.Gate), the MERGED
// table NewManager builds (uiws.CoreLevels ∪ the document wsLevels) and the super-user
// set must be byte-for-byte what the single pre-extraction internal/ui.wsLevels /
// wsSuperUserOps held — so every /ws/ui message gates at exactly the same level/scope
// as before. expectedCombined / expectedSuper below are the frozen pre-extraction
// tables, transcribed verbatim (values unchanged); the test fails if any row drifts.
func TestGateTables_MatchPreExtraction(t *testing.T) {
	expectedCombined := map[MessageType]uiws.Op{
		// --- document rows (unchanged; never moved) ---
		GetProjects:    {Level: authz.LevelRead, Global: true},
		GetTree:        {Level: authz.LevelRead, Global: false},
		ReadFile:       {Level: authz.LevelRead, Global: false},
		MsgSearchFiles: {Level: authz.LevelRead, Global: false},
		MsgGetHistory:  {Level: authz.LevelRead, Global: false},
		MsgGetFileAt:   {Level: authz.LevelRead, Global: false},
		MsgGetDiff:     {Level: authz.LevelRead, Global: false},

		WriteDraft:    {Level: authz.LevelWrite, Global: false},
		SaveFile:      {Level: authz.LevelWrite, Global: false},
		MsgMoveFile:   {Level: authz.LevelWrite, Global: false},
		MsgDeleteFile: {Level: authz.LevelWrite, Global: false},

		MsgCreateProject: {Level: authz.LevelAdmin, Global: false},
		MsgDeleteProject: {Level: authz.LevelAdmin, Global: false},
		MsgRenameProject: {Level: authz.LevelAdmin, Global: false},

		MsgRecoverProject: {Level: authz.LevelAdmin, Global: false},

		MsgListDeleted: {Level: authz.LevelAdmin, Global: false},
		MsgReviveFile:  {Level: authz.LevelAdmin, Global: false},

		MsgNamespaceHealth:  {Level: authz.LevelAdmin, Global: true},
		MsgNamespaceRecover: {Level: authz.LevelAdmin, Global: false},

		// --- document rows added after the extraction (B-73 librarian status) ---
		MsgLibrarianStatus:        {Level: authz.LevelAdmin, Global: true},
		MsgRefreshLibrarianStatus: {Level: authz.LevelAdmin, Global: true},

		// --- core rows (moved to uiws.CoreLevels; values must be unchanged) ---
		uiws.MsgOAuthList:      {Level: authz.LevelAdmin, Global: true},
		uiws.MsgOAuthRevoke:    {Level: authz.LevelAdmin, Global: true},
		uiws.MsgOAuthIssueSelf: {Level: authz.LevelAdmin, Global: true},

		uiws.MsgDomainList:            {Level: authz.LevelAdmin, Global: true},
		uiws.MsgDomainCreate:          {Level: authz.LevelAdmin, Global: true},
		uiws.MsgDomainUpdate:          {Level: authz.LevelAdmin, Global: true},
		uiws.MsgDomainDelete:          {Level: authz.LevelAdmin, Global: true},
		uiws.MsgDomainGenerateConsent: {Level: authz.LevelAdmin, Global: true},
		uiws.MsgClientIssue:           {Level: authz.LevelAdmin, Global: true},
		uiws.MsgClientList:            {Level: authz.LevelAdmin, Global: true},
		uiws.MsgClientRevoke:          {Level: authz.LevelAdmin, Global: true},

		uiws.MsgAccountGet:         {Level: authz.LevelRead, Global: true},
		uiws.MsgAccountSetName:     {Level: authz.LevelRead, Global: true},
		uiws.MsgAccountSetPassword: {Level: authz.LevelRead, Global: true},

		uiws.MsgAdminListUsers:       {Level: authz.LevelAdmin, Global: true},
		uiws.MsgAdminSetUserScope:    {Level: authz.LevelAdmin, Global: true},
		uiws.MsgAdminSetUserEnabled:  {Level: authz.LevelAdmin, Global: true},
		uiws.MsgAdminSetUserPassword: {Level: authz.LevelAdmin, Global: true},
		uiws.MsgAdminRemoveUser:      {Level: authz.LevelAdmin, Global: true},
		uiws.MsgAdminCreateInvite:    {Level: authz.LevelAdmin, Global: true},
		uiws.MsgAdminListInvites:     {Level: authz.LevelAdmin, Global: true},
		uiws.MsgAdminRevokeInvite:    {Level: authz.LevelAdmin, Global: true},
	}
	expectedSuper := map[MessageType]bool{
		MsgCreateNamespace: true,
		MsgDeleteNamespace: true,
		MsgMoveProject:     true,
		MsgRenameNamespace: true,
	}

	m := NewManager(nil, nil, nil)

	if !reflect.DeepEqual(m.levels, expectedCombined) {
		t.Errorf("merged gate level table drifted from the pre-extraction wsLevels.\n got=%v\nwant=%v", m.levels, expectedCombined)
	}
	if !reflect.DeepEqual(m.superOps, expectedSuper) {
		t.Errorf("super-user op set drifted from the pre-extraction wsSuperUserOps.\n got=%v\nwant=%v", m.superOps, expectedSuper)
	}

	// The merge must be the disjoint UNION of the two sub-tables (no key collision
	// silently overwriting a row), so the split reproduces one combined table exactly.
	if got, want := len(m.levels), len(uiws.CoreLevels)+len(wsLevels); got != want {
		t.Errorf("merged table size %d != len(CoreLevels)+len(wsLevels) %d — the core and document rows are not disjoint", got, want)
	}
}
