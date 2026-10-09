package db

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// twoWorkspaces builds two households, each with one list, section and item.
type household struct {
	workspace Workspace
	user      *User
	list      *List
	section   *Section
	item      *Item
}

func newHousehold(t *testing.T, name string) household {
	t.Helper()
	user, err := CreateUser(name+"-user", name, "correct horse battery", false)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := CreateWorkspace(name, "", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := CreateList(workspace.ID, name+" list", "")
	if err != nil {
		t.Fatal(err)
	}
	section, err := CreateSectionForList(list.ID, name+" section")
	if err != nil {
		t.Fatal(err)
	}
	item, err := CreateItem(section.ID, name+" milk", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveItemHistory(workspace.ID, item.Name, section.ID); err != nil {
		t.Fatal(err)
	}
	return household{*workspace, user, list, section, item}
}

func TestQueriesAreScopedToTheWorkspace(t *testing.T) {
	initOfflineDatabase(t)
	a, b := newHousehold(t, "alpha"), newHousehold(t, "beta")

	for _, h := range []household{a, b} {
		lists, err := GetAllLists(h.workspace.ID)
		if err != nil || len(lists) != 1 || lists[0].ID != h.list.ID {
			t.Fatalf("%s sees lists %#v (err %v)", h.workspace.Name, lists, err)
		}
		suggestions, err := GetAllItemSuggestions(h.workspace.ID, 50)
		if err != nil || len(suggestions) != 1 || suggestions[0].Name != h.item.Name {
			t.Fatalf("%s sees suggestions %#v (err %v)", h.workspace.Name, suggestions, err)
		}
		history, _ := GetItemHistoryList(h.workspace.ID)
		if len(history) != 1 {
			t.Fatalf("%s sees %d history entries", h.workspace.Name, len(history))
		}
	}

	// The same name can exist in both workspaces.
	if exists, _ := ListNameExists(a.workspace.ID, b.list.Name, 0); exists {
		t.Fatal("list names must be unique per workspace, not globally")
	}
	if _, err := CreateList(a.workspace.ID, b.list.Name, ""); err != nil {
		t.Fatal(err)
	}

	// Activating a list only affects its own workspace.
	if err := SetActiveList(a.list.ID); err != nil {
		t.Fatal(err)
	}
	if err := SetActiveList(b.list.ID); err != nil {
		t.Fatal(err)
	}
	if active, err := GetActiveList(a.workspace.ID); err != nil || active.ID != a.list.ID {
		t.Fatalf("activating in workspace B changed workspace A: %v %v", active, err)
	}

	// Ownership lookups feed the HTTP guards.
	if WorkspaceOfItem(a.item.ID) != a.workspace.ID || WorkspaceOfSection(b.section.ID) != b.workspace.ID || WorkspaceOfList(a.list.ID) == b.workspace.ID {
		t.Fatal("ownership lookups return the wrong workspace")
	}

	// Clearing one workspace leaves the other intact.
	if err := ClearWorkspaceData(a.workspace.ID); err != nil {
		t.Fatal(err)
	}
	if lists, _ := GetAllLists(b.workspace.ID); len(lists) != 1 {
		t.Fatal("clearing workspace A removed workspace B's lists")
	}
	if history, _ := GetItemHistoryList(b.workspace.ID); len(history) != 1 {
		t.Fatal("clearing workspace A removed workspace B's history")
	}

	// Deleting history is scoped as well.
	bHistory, _ := GetItemHistoryList(b.workspace.ID)
	if err := DeleteItemHistory(a.workspace.ID, bHistory[0].ID); err == nil {
		t.Fatal("a workspace deleted another workspace's history entry")
	}
}

func TestOfflineSyncCannotTouchAnotherWorkspace(t *testing.T) {
	initOfflineDatabase(t)
	a, b := newHousehold(t, "alpha"), newHousehold(t, "beta")
	scopeA := a.workspace.ID

	attempts := map[string]OfflineOperation{
		"update foreign item":    offlineOperation("item", "update", b.item.ID, map[string]interface{}{"name": "hijacked"}),
		"update foreign section": offlineOperation("section", "update", b.section.ID, map[string]interface{}{"name": "hijacked"}),
		"update foreign list":    offlineOperation("list", "update", b.list.ID, map[string]interface{}{"name": "hijacked"}),
		"item into foreign section": offlineOperation("item", "create", -1,
			map[string]interface{}{"name": "planted", "section_id": b.section.ID}),
		"section into foreign list": offlineOperation("section", "create", -1,
			map[string]interface{}{"name": "planted", "list_id": b.list.ID}),
		"move own item to foreign section": offlineOperation("item", "update", a.item.ID,
			map[string]interface{}{"section_id": b.section.ID}),
	}
	for name, operation := range attempts {
		_, err := ApplyOfflineSync(scopeA, OfflineSyncRequest{ClientID: uuid.NewString(), Operations: []OfflineOperation{operation}}, nil)
		var inputErr *OfflineError
		if !errors.As(err, &inputErr) || !inputErr.Conflict {
			t.Errorf("%s: expected a conflict, got %v", name, err)
		}
	}

	// Deleting a foreign entity is indistinguishable from deleting a missing one
	// and must not remove anything.
	for _, operation := range []OfflineOperation{
		offlineOperation("item", "delete", b.item.ID, nil),
		offlineOperation("section", "delete", b.section.ID, nil),
		offlineOperation("list", "delete", b.list.ID, nil),
	} {
		if _, err := ApplyOfflineSync(scopeA, OfflineSyncRequest{ClientID: uuid.NewString(), Operations: []OfflineOperation{operation}}, nil); err != nil {
			t.Errorf("foreign delete should be a silent no-op, got %v", err)
		}
	}

	snapshotB, err := GetOfflineSnapshot(b.workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshotB.Lists) != 1 || len(snapshotB.Sections) != 1 || len(snapshotB.Items) != 1 || snapshotB.Items[0].Name != b.item.Name {
		t.Fatalf("workspace B was modified: %#v", snapshotB)
	}
}

func TestOfflineSyncCreatesInsideTheCallersWorkspace(t *testing.T) {
	initOfflineDatabase(t)
	a, b := newHousehold(t, "alpha"), newHousehold(t, "beta")
	scopeA := a.workspace.ID

	response, err := ApplyOfflineSync(scopeA, OfflineSyncRequest{ClientID: uuid.NewString(), Operations: offlineCreateTree()}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The response snapshot only contains workspace A's data.
	if len(response.Snapshot.Lists) != 2 {
		t.Fatalf("snapshot lists = %d", len(response.Snapshot.Lists))
	}
	if got := WorkspaceOfList(response.Results[0].ServerID); got != a.workspace.ID {
		t.Fatalf("list created in workspace %d, want %d", got, a.workspace.ID)
	}
	snapshotB, _ := GetOfflineSnapshot(b.workspace.ID)
	if len(snapshotB.Lists) != 1 || len(snapshotB.Items) != 1 {
		t.Fatal("offline creates leaked into workspace B")
	}
	// Autocomplete history stays with the workspace that created the item.
	if history, _ := GetItemHistoryList(b.workspace.ID); len(history) != 1 {
		t.Fatalf("workspace B history has %d entries", len(history))
	}
	if history, _ := GetItemHistoryList(a.workspace.ID); len(history) != 2 {
		t.Fatalf("workspace A history has %d entries, want 2", len(history))
	}
}

func TestMembershipRules(t *testing.T) {
	initOfflineDatabase(t)
	a := newHousehold(t, "alpha")
	guest, err := CreateUser("guest", "Guest", "correct horse battery", false)
	if err != nil {
		t.Fatal(err)
	}
	if MemberRole(a.workspace.ID, guest.ID) != "" {
		t.Fatal("a stranger must not be a member")
	}
	if id, _ := ResolveSessionWorkspace(guest.ID, a.workspace.ID); id != 0 {
		t.Fatalf("a non-member session resolved to workspace %d", id)
	}
	if err := AddMember(a.workspace.ID, guest.ID, RoleMember); err != nil {
		t.Fatal(err)
	}
	if id, _ := ResolveSessionWorkspace(guest.ID, a.workspace.ID); id != a.workspace.ID {
		t.Fatal("a member should resolve to the workspace")
	}
	// The last owner cannot leave; a plain member can.
	if err := RemoveMember(a.workspace.ID, a.user.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("removing the last owner: %v", err)
	}
	if err := RemoveMember(a.workspace.ID, guest.ID); err != nil {
		t.Fatal(err)
	}
	if MemberRole(a.workspace.ID, guest.ID) != "" {
		t.Fatal("member was not removed")
	}
	if err := DeleteWorkspace(DefaultWorkspaceID); err == nil {
		t.Fatal("the default workspace must not be deletable")
	}
}

func TestAuthenticate(t *testing.T) {
	initOfflineDatabase(t)
	if _, err := CreateUser("Artur", "Artur", "correct horse battery", true); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateUser("artur", "Other", "another password", false); !errors.Is(err, ErrUserExists) {
		t.Fatalf("usernames are case-insensitive, got %v", err)
	}
	if u, err := Authenticate("ARTUR", "correct horse battery"); err != nil || !u.IsAdmin {
		t.Fatalf("valid login failed: %v", err)
	}
	for _, c := range [][2]string{{"artur", "wrong"}, {"nobody", "correct horse battery"}, {"artur", ""}} {
		if _, err := Authenticate(c[0], c[1]); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("Authenticate(%q, %q) = %v", c[0], c[1], err)
		}
	}
}

func TestLegacyDataMovesToTheDefaultWorkspace(t *testing.T) {
	initOfflineDatabase(t)
	// A list created the old way (no workspace given) belongs to workspace 1.
	if _, err := DB.Exec(`INSERT INTO lists (name, icon, sort_order) VALUES ('Old list', '🛒', 0)`); err != nil {
		t.Fatal(err)
	}
	lists, err := GetAllLists(DefaultWorkspaceID)
	if err != nil || len(lists) != 1 {
		t.Fatalf("legacy list not in the default workspace: %v %v", lists, err)
	}
	if w, err := GetWorkspace(DefaultWorkspaceID); err != nil || w.Name != "Home" {
		t.Fatalf("default workspace missing: %v %v", w, err)
	}
}

func TestListMovesStayInsideTheirWorkspace(t *testing.T) {
	initOfflineDatabase(t)
	a, b := newHousehold(t, "alpha"), newHousehold(t, "beta")
	a2, err := CreateList(a.workspace.ID, "alpha two", "")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := CreateList(b.workspace.ID, "beta two", "")
	if err != nil {
		t.Fatal(err)
	}
	orders := func(workspaceID int64) []string {
		lists, _ := GetAllLists(workspaceID)
		var out []string
		for _, l := range lists {
			out = append(out, l.Name)
		}
		return out
	}
	betaBefore := orders(b.workspace.ID)
	betaSortOrders := func() [2]int {
		lists, _ := GetAllLists(b.workspace.ID)
		return [2]int{lists[0].SortOrder, lists[1].SortOrder}
	}
	before := betaSortOrders()

	if err := MoveListUp(a2.ID); err != nil {
		t.Fatal(err)
	}
	if got := orders(a.workspace.ID); got[0] != "alpha two" || got[1] != "alpha list" {
		t.Fatalf("alpha order after MoveListUp = %v", got)
	}
	if err := MoveListDown(a2.ID); err != nil {
		t.Fatal(err)
	}
	if got := orders(a.workspace.ID); got[0] != "alpha list" || got[1] != "alpha two" {
		t.Fatalf("alpha order after MoveListDown = %v", got)
	}
	if got := betaSortOrders(); got != before {
		t.Fatalf("moving alpha's lists changed beta's sort_order: %v -> %v", before, got)
	}
	if got := orders(b.workspace.ID); got[0] != betaBefore[0] || got[1] != betaBefore[1] {
		t.Fatalf("beta order changed: %v", got)
	}

	// The last list of a small workspace is still "at the bottom" even though
	// another workspace has more lists.
	if _, err := CreateList(a.workspace.ID, "alpha three", ""); err != nil {
		t.Fatal(err)
	}
	if err := MoveListDown(b2.ID); err != nil {
		t.Fatal(err)
	}
	if got := orders(b.workspace.ID); got[1] != "beta two" {
		t.Fatalf("MoveListDown on the last list moved it: %v", got)
	}
}

func TestSectionLookupByNameStaysInsideTheWorkspace(t *testing.T) {
	initOfflineDatabase(t)
	a, b := newHousehold(t, "alpha"), newHousehold(t, "beta")
	if _, err := CreateSectionForList(b.list.ID, "alpha section"); err != nil {
		t.Fatal(err)
	}
	// Look everything up first: the single database connection is held by the
	// transaction, so plain queries (WorkspaceOfSection) must wait until it ends.
	tx, err := DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	inA := GetSectionIDByNameTx(tx, a.workspace.ID, "alpha section")
	inB := GetSectionIDByNameTx(tx, b.workspace.ID, "alpha section")
	ownB := GetSectionIDByNameTx(tx, b.workspace.ID, "beta section")
	foreign := GetSectionIDByNameTx(tx, a.workspace.ID, "beta section")
	tx.Rollback()

	if WorkspaceOfSection(inA) != a.workspace.ID || WorkspaceOfSection(inB) != b.workspace.ID || inA == inB {
		t.Fatalf("section lookups crossed workspaces: %d in %d, %d in %d", inA, WorkspaceOfSection(inA), inB, WorkspaceOfSection(inB))
	}
	if WorkspaceOfSection(ownB) != b.workspace.ID {
		t.Fatal("own section not found")
	}
	if foreign != 0 {
		t.Fatalf("a foreign section name resolved to %d", foreign)
	}
}

func TestHistoryIsWrittenToTheGivenWorkspace(t *testing.T) {
	initOfflineDatabase(t)
	a, b := newHousehold(t, "alpha"), newHousehold(t, "beta")

	// No section (import without a last section, REST API history entry) must
	// still land in the caller's workspace, not in workspace 1.
	if err := SaveItemHistory(b.workspace.ID, "no section item", 0); err != nil {
		t.Fatal(err)
	}
	tx, _ := DB.Begin()
	if err := SaveItemHistoryWithCountTx(tx, b.workspace.ID, "counted item", 0, 5); err != nil {
		t.Fatal(err)
	}
	SaveItemHistoryTx(tx, b.workspace.ID, "tx item", 0)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	names := func(workspaceID int64) map[string]bool {
		out := map[string]bool{}
		history, _ := GetItemHistoryList(workspaceID)
		for _, h := range history {
			out[h.Name] = true
		}
		return out
	}
	for _, name := range []string{"no section item", "counted item", "tx item"} {
		if !names(b.workspace.ID)[name] {
			t.Errorf("%q missing from workspace B", name)
		}
		if names(a.workspace.ID)[name] || names(DefaultWorkspaceID)[name] {
			t.Errorf("%q leaked into another workspace", name)
		}
	}
}

func TestApplyingATemplateRecordsHistoryInTheListsWorkspace(t *testing.T) {
	initOfflineDatabase(t)
	a, b := newHousehold(t, "alpha"), newHousehold(t, "beta")
	template, err := CreateTemplate(b.workspace.ID, "weekly", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddTemplateItem(template.ID, "Dairy", "Kefir", ""); err != nil {
		t.Fatal(err)
	}
	if err := ApplyTemplateToList(template.ID, b.list.ID); err != nil {
		t.Fatal(err)
	}
	// Applying it twice bumps the usage count instead of failing.
	if err := ApplyTemplateToList(template.ID, b.list.ID); err != nil {
		t.Fatal(err)
	}
	var kefir *HistoryItem
	history, _ := GetItemHistoryList(b.workspace.ID)
	for i := range history {
		if history[i].Name == "Kefir" {
			kefir = &history[i]
		}
	}
	if kefir == nil || kefir.UsageCount != 2 {
		t.Fatalf("template items should be remembered in the list's workspace: %#v", history)
	}
	if h, _ := GetItemHistoryList(a.workspace.ID); len(h) != 1 {
		t.Fatalf("workspace A history changed: %#v", h)
	}
}

func TestAddingAMemberNeverDemotesAnOwner(t *testing.T) {
	initOfflineDatabase(t)
	a := newHousehold(t, "alpha")
	guest, err := CreateUser("guest", "Guest", "correct horse battery", false)
	if err != nil {
		t.Fatal(err)
	}
	// The owner adds themselves again without ticking "as owner".
	if err := AddMember(a.workspace.ID, a.user.ID, RoleMember); err != nil {
		t.Fatal(err)
	}
	if MemberRole(a.workspace.ID, a.user.ID) != RoleOwner {
		t.Fatal("re-adding the owner as a member demoted them")
	}
	// A member can be promoted by adding them as an owner.
	if err := AddMember(a.workspace.ID, guest.ID, RoleMember); err != nil {
		t.Fatal(err)
	}
	if err := AddMember(a.workspace.ID, guest.ID, RoleOwner); err != nil {
		t.Fatal(err)
	}
	if MemberRole(a.workspace.ID, guest.ID) != RoleOwner {
		t.Fatal("promotion to owner did not apply")
	}
	if err := AddMember(a.workspace.ID, guest.ID, RoleMember); err != nil {
		t.Fatal(err)
	}
	if MemberRole(a.workspace.ID, guest.ID) != RoleOwner {
		t.Fatal("adding again demoted the co-owner")
	}
}

func TestNoActiveListFallbacksStayInsideTheWorkspace(t *testing.T) {
	initOfflineDatabase(t)
	a, b := newHousehold(t, "alpha"), newHousehold(t, "beta")
	// Neither workspace has an active list yet.
	sections, err := GetAllSections(b.workspace.ID)
	if err != nil || len(sections) != 1 || sections[0].ID != b.section.ID || len(sections[0].Items) != 1 {
		t.Fatalf("beta sections without an active list = %#v (%v)", sections, err)
	}
	if stats := GetStats(a.workspace.ID); stats.TotalItems != 1 {
		t.Fatalf("alpha stats without an active list = %#v", stats)
	}
	if stats := GetStats(999); stats.TotalItems != 0 {
		t.Fatalf("an unknown workspace has stats: %#v", stats)
	}
}

func TestWorkspaceIcons(t *testing.T) {
	initOfflineDatabase(t)
	user, err := CreateUser("icons", "Icons", "correct horse battery", false)
	if err != nil {
		t.Fatal(err)
	}

	// A workspace made by an older release, or without a choice, gets the default.
	plain, err := CreateWorkspace("plain", "", user.ID)
	if err != nil || plain.Icon != DefaultWorkspaceIcon {
		t.Fatalf("no icon given: %+v, %v", plain, err)
	}
	if home, err := GetWorkspace(DefaultWorkspaceID); err != nil || home.Icon != DefaultWorkspaceIcon {
		t.Fatalf("the default workspace has icon %q (%v)", home.Icon, err)
	}

	chosen, err := CreateWorkspace("grandparents", "👵", user.ID)
	if err != nil || chosen.Icon != "👵" {
		t.Fatalf("chosen icon: %+v, %v", chosen, err)
	}
	listed, _ := WorkspacesForUser(user.ID)
	icons := map[string]string{}
	for _, w := range listed {
		icons[w.Name] = w.Icon
	}
	if icons["grandparents"] != "👵" || icons["plain"] != DefaultWorkspaceIcon {
		t.Fatalf("listed icons: %v", icons)
	}

	if err := UpdateWorkspace(chosen.ID, "  Grandparents  ", "🐕"); err != nil {
		t.Fatal(err)
	}
	if got, _ := GetWorkspace(chosen.ID); got.Name != "Grandparents" || got.Icon != "🐕" {
		t.Fatalf("after update: %+v", got)
	}
	if err := UpdateWorkspace(chosen.ID, "Grandparents", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := GetWorkspace(chosen.ID); got.Icon != DefaultWorkspaceIcon {
		t.Fatalf("clearing the icon should restore the default, got %q", got.Icon)
	}
	all, _ := AllWorkspaces()
	for _, w := range all {
		if w.Icon == "" {
			t.Fatalf("workspace %q has no icon", w.Name)
		}
	}
}

func TestDeletingTheOnlyOwnerOfASharedWorkspaceIsRefused(t *testing.T) {
	initOfflineDatabase(t)
	shared := newHousehold(t, "shared")
	member, err := CreateUser("member", "Member", "correct horse battery", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddMember(shared.workspace.ID, member.ID, RoleMember); err != nil {
		t.Fatal(err)
	}

	// Others would be left in a workspace nobody can manage.
	if err := DeleteUser(shared.user.ID); !errors.Is(err, ErrSoleOwner) {
		t.Fatalf("deleting the only owner: %v, want ErrSoleOwner", err)
	}
	if _, err := GetUserByID(shared.user.ID); err != nil {
		t.Fatal("the refused deletion still removed the account")
	}
	// A member can be deleted freely.
	spare, _ := CreateUser("spare", "Spare", "correct horse battery", false)
	AddMember(shared.workspace.ID, spare.ID, RoleMember)
	if err := DeleteUser(spare.ID); err != nil {
		t.Fatalf("deleting a plain member: %v", err)
	}
	// Once someone else owns it too, the first owner can go.
	if err := AddMember(shared.workspace.ID, member.ID, RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := DeleteUser(shared.user.ID); err != nil {
		t.Fatalf("deleting one of two owners: %v", err)
	}

	// A workspace nobody else is in is not blocked: only its owner is affected.
	alone := newHousehold(t, "alone")
	if err := DeleteUser(alone.user.ID); err != nil {
		t.Fatalf("deleting the owner of a workspace with no other members: %v", err)
	}
}

// bcrypt only looks at the first 72 bytes. Longer passwords are refused, and a
// login attempt that merely starts with the stored password must not match.
func TestPasswordsBeyondWhatBcryptUsesAreNeverTruncated(t *testing.T) {
	initOfflineDatabase(t)
	long := strings.Repeat("b", MaxPasswordBytes)
	if _, err := CreateUser("tooLong", "Too long", long+"x", false); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("73 bytes: %v, want ErrPasswordTooLong", err)
	}
	if _, err := CreateUser("exact", "Exact", long, false); err != nil {
		t.Fatalf("72 bytes: %v", err)
	}
	if _, err := Authenticate("exact", long); err != nil {
		t.Fatalf("the exact password: %v", err)
	}
	if _, err := Authenticate("exact", long+"EXTRA"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("a password that only starts with the stored one was accepted: %v", err)
	}

	// The same holds when a password is changed or checked later.
	user, _ := GetUserByUsername("exact")
	if CheckPassword(user.ID, long+"EXTRA") {
		t.Fatal("CheckPassword accepted a password that only starts with the stored one")
	}
	if err := SetPassword(user.ID, long+"x", ""); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("SetPassword with 73 bytes: %v, want ErrPasswordTooLong", err)
	}
	if !CheckPassword(user.ID, long) {
		t.Fatal("a refused SetPassword changed the password")
	}
}
