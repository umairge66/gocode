package settingsload

import (
	"encoding/binary"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

func strp(s string) *string { return &s }

func TestExtractRegistryValue(t *testing.T) {
	out := "\r\nHKEY_LOCAL_MACHINE\\SOFTWARE\\Policies\\gocode\r\n    Settings    REG_SZ    {\"model\":\"opus\"}  \r\n\r\n"
	v, ok := ExtractRegistryValue(out)
	if !ok || v != `{"model":"opus"}` {
		t.Fatalf("REG_SZ: got %q %v", v, ok)
	}
	v, ok = ExtractRegistryValue("\n    settings\tREG_EXPAND_SZ\t{\"a\":1}\n")
	if !ok || v != `{"a":1}` {
		t.Fatalf("REG_EXPAND_SZ (case-insensitive): got %q %v", v, ok)
	}
	if _, ok := ExtractRegistryValue("    Settings    REG_DWORD    0x1\n"); ok {
		t.Fatal("REG_DWORD must not match")
	}
	if _, ok := ExtractRegistryValue("Settings    REG_SZ    x\n"); ok {
		t.Fatal("the value line must be indented")
	}
}

func TestReadoutFromRegQueries(t *testing.T) {
	r := readoutFromRegQueries(
		regQueryResult{ok: true, stdout: "    Settings    REG_SZ    {\"a\":1}\n"},
		regQueryResult{},
	)
	if r.HKLM == nil || *r.HKLM != `{"a":1}` || r.HKCU != nil || r.HKLMUnreadReason != "" {
		t.Fatalf("got %+v", r)
	}
	r = readoutFromRegQueries(regQueryResult{oversize: true}, regQueryResult{ok: true, stdout: "nothing"})
	if r.HKLM != nil || r.HKLMUnreadReason != "the value exceeds 2 MiB" || r.HKCU == nil || *r.HKCU != "" {
		t.Fatalf("got %+v", r)
	}
	if d := ParseManagedDocument(*r.HKCU, "x", true); d.LoadState != settingspolicy.LoadDidNotLoad {
		t.Fatalf("empty registry value must not parse: %+v", d)
	}
}

func TestParseManagedDocumentUserWritable(t *testing.T) {
	d := ParseManagedDocument(`{"model":"x","managedMcpServers":{},"isolation":1,"appendPlugins":[],
		"enabledPlugins":{"a@m":true,"b@m":false,"c@builtin":false}}`, "HKCU", true)
	if d.LoadState != settingspolicy.LoadLoaded || !d.HasPolicyContent {
		t.Fatalf("state: %+v", d)
	}
	for _, k := range []string{"managedMcpServers", "isolation", "appendPlugins"} {
		if _, ok := d.Settings[k]; ok {
			t.Errorf("%s must be stripped from a user-writable document", k)
		}
	}
	if !reflect.DeepEqual(d.Settings["enabledPlugins"], map[string]any{"b@m": false}) {
		t.Errorf("enabledPlugins = %v", d.Settings["enabledPlugins"])
	}
	if len(d.Errors) != 4 {
		t.Fatalf("want 4 warnings, got %v", d.Errors)
	}
	for _, e := range d.Errors {
		if e.Severity != "warning" {
			t.Errorf("user-writable diagnostics are warnings: %+v", e)
		}
	}

	admin := ParseManagedDocument(`{"managedMcpServers":{}}`, "HKLM", false)
	if _, ok := admin.Settings["managedMcpServers"]; !ok {
		t.Error("admin documents keep admin-only keys")
	}
	ctl := ParseManagedDocument(`{"wslInheritsWindowsSettings":true,"managedSourcesBehavior":"merge"}`, "HKLM", false)
	if ctl.HasPolicyContent {
		t.Error("control keys alone are not policy content")
	}
	bad := ParseManagedDocument(`[1]`, "HKLM", false)
	if bad.LoadState != settingspolicy.LoadDidNotLoad || bad.Errors[0].Severity != "fatal" {
		t.Errorf("non-object admin document: %+v", bad)
	}
}

func TestComposePlistPrecedence(t *testing.T) {
	fsys := fsfacade.NewMemFS()
	ctx := newCtx()
	r := OSPolicyReadout{Plists: []PlistDocument{
		{Label: "user-mdm", JSON: strp(`{"managedSourcesBehavior":"merge"}`)}, // no content: falls through
		{Label: "device", JSON: strp(`{"model":"device"}`)},
		{Label: "later", JSON: strp(`{"model":"later"}`)},
	}}
	p := ComposeOSPolicy(r, fsys, ctx)
	if p.MDM.Settings["model"] != "device" || !p.MDM.HasPolicyContent {
		t.Fatalf("first document with content wins: %+v", p.MDM)
	}

	// An unreadable admin document holds the slot against a later
	// user-writable one, and its error is reported.
	r = OSPolicyReadout{Plists: []PlistDocument{
		{Label: "admin", UnreadReason: "boom"},
		{Label: "user", JSON: strp(`{"model":"user"}`), UserWritable: true},
	}}
	p = ComposeOSPolicy(r, fsys, ctx)
	if p.MDM.HasPolicyContent || len(p.MDM.Errors) != 1 || p.MDM.LoadState != settingspolicy.LoadDidNotLoad ||
		!strings.Contains(p.MDM.Errors[0].Message, "could not be read: boom") {
		t.Fatalf("got %+v", p.MDM)
	}
	// Without the admin document the user-writable one is used.
	r.Plists = r.Plists[1:]
	if p = ComposeOSPolicy(r, fsys, ctx); p.MDM.Settings["model"] != "user" || !p.MDM.UserWritable {
		t.Fatalf("got %+v", p.MDM)
	}
}

func TestComposeNativeRegistry(t *testing.T) {
	fsys := fsfacade.NewMemFS()
	ctx := newCtx()

	p := ComposeOSPolicy(OSPolicyReadout{HKLM: strp(`{"model":"hklm"}`), HKCU: strp(`{"model":"hkcu"}`)}, fsys, ctx)
	if p.MDM.Settings["model"] != "hklm" || p.HKCU != nil {
		t.Fatalf("HKLM wins and suppresses HKCU: %+v", p)
	}

	p = ComposeOSPolicy(OSPolicyReadout{HKCU: strp(`{"model":"hkcu","wslInheritsWindowsSettings":true,"isolation":1}`)}, fsys, ctx)
	if p.MDM.HasPolicyContent || !reflect.DeepEqual(p.HKCU, map[string]any{"model": "hkcu"}) {
		t.Fatalf("HKCU alone applies, control and admin-only keys dropped: %+v", p)
	}

	// A deployed managed file outranks HKCU.
	fsys.WriteFile("/etc/gocode/managed-settings.json", []byte(`{"model":"file"}`))
	p = ComposeOSPolicy(OSPolicyReadout{HKCU: strp(`{"model":"hkcu"}`)}, fsys, ctx)
	if p.HKCU != nil {
		t.Fatalf("managed file must suppress HKCU: %+v", p)
	}

	// Unreadable HKLM value holds the slot.
	p = ComposeOSPolicy(OSPolicyReadout{HKLMUnreadReason: registryUnreadOversize}, fsys, ctx)
	if p.MDM.LoadState != settingspolicy.LoadDidNotLoad || len(p.MDM.Errors) != 1 {
		t.Fatalf("got %+v", p.MDM)
	}
}

func TestComposeWSLBridge(t *testing.T) {
	ctx := newCtx()
	ctx.WSLWindowsManagedDirOverride = "/mnt/c/Program Files/gocode"

	// Not armed: HKLM is ignored on WSL.
	fsys := fsfacade.NewMemFS()
	p := ComposeOSPolicy(OSPolicyReadout{IsWSL: true, HKLM: strp(`{"model":"hklm"}`)}, fsys, ctx)
	if p.MDM.HasPolicyContent || p.WSLInherits {
		t.Fatalf("unarmed WSL must ignore HKLM: %+v", p)
	}

	// Armed in HKLM itself.
	p = ComposeOSPolicy(OSPolicyReadout{IsWSL: true, HKLM: strp(`{"model":"hklm","wslInheritsWindowsSettings":true}`)}, fsys, ctx)
	if p.MDM.Settings["model"] != "hklm" || !p.WSLInherits {
		t.Fatalf("armed WSL takes HKLM: %+v", p)
	}

	// Armed through the DrvFs folder; HKCU needs its own opt-in.
	fsys.WriteFile("/mnt/c/Program Files/gocode/managed-settings.d/10-flag.json", []byte(`{"wslInheritsWindowsSettings":"true"}`))
	p = ComposeOSPolicy(OSPolicyReadout{IsWSL: true, HKCU: strp(`{"model":"hkcu"}`)}, fsys, ctx)
	if !p.WSLInherits || p.HKCU != nil {
		t.Fatalf("HKCU without its own flag must not apply on WSL: %+v", p)
	}
	// The drop-in only carries the flag (no policy content), so HKCU with the
	// double opt-in applies.
	p = ComposeOSPolicy(OSPolicyReadout{IsWSL: true, HKCU: strp(`{"model":"hkcu","wslInheritsWindowsSettings":true}`)}, fsys, ctx)
	if p.HKCU["model"] != "hkcu" {
		t.Fatalf("double opt-in HKCU applies: %+v", p)
	}

	// Unreadable flag fails closed: an empty MDM tier holding the slot.
	fsys = fsfacade.NewMemFS()
	p = ComposeOSPolicy(OSPolicyReadout{IsWSL: true, HKLM: strp(`{"wslInheritsWindowsSettings":"yes"}`)}, fsys, ctx)
	if !p.MDM.HasPolicyContent || len(p.MDM.Settings) != 0 || !p.WSLInherits {
		t.Fatalf("unreadable flag must fail closed: %+v", p)
	}
	last := p.MDM.Errors[len(p.MDM.Errors)-1]
	if last.Path != "wslInheritsWindowsSettings" || last.Severity != "fatal" {
		t.Fatalf("missing fail-closed diagnostic: %+v", p.MDM.Errors)
	}
}

func TestAdminTiersOSPrecedence(t *testing.T) {
	ctx := newCtx()
	ctx.WSLWindowsManagedDirOverride = "/win"
	fsys := fsfacade.NewMemFS()
	fsys.WriteFile("/etc/gocode/managed-settings.json", []byte(`{"tier":"etc"}`))
	fsys.WriteFile("/win/managed-settings.json", []byte(`{"tier":"win"}`))

	// WSL inheriting Windows policy: HKLM is the MDM tier and the Windows
	// folder (which holds the slot) is the file tier; /etc/gocode is not read.
	ctx.OSPolicyReader = func() OSPolicyReadout {
		return OSPolicyReadout{IsWSL: true, HKLM: strp(`{"tier":"hklm","wslInheritsWindowsSettings":true}`)}
	}
	p := ComposePolicy(fsys, ctx)
	var got []any
	for _, tr := range p.Tiers {
		got = append(got, tr["tier"])
	}
	if !reflect.DeepEqual(got, []any{"hklm", "win"}) || !reflect.DeepEqual(p.TierSources, []string{"hklm", "file"}) {
		t.Fatalf("tier order = %v %v; want MDM > Windows folder", got, p.TierSources)
	}
	if p.Settings["tier"] != "hklm" || p.Merged {
		t.Fatalf("first-wins picks MDM: %v", p.Settings)
	}

	// No MDM, no files: HKCU is the policy.
	fsys = fsfacade.NewMemFS()
	ctx.OSPolicyReader = func() OSPolicyReadout { return OSPolicyReadout{HKCU: strp(`{"tier":"hkcu"}`)} }
	if p := ComposePolicy(fsys, ctx); !p.FromHKCU || p.Settings["tier"] != "hkcu" {
		t.Fatalf("HKCU policy: %+v", p)
	}

	// Fail-closed: the unreadable WSL flag holds the slot with an empty
	// policy, so neither the managed file nor HKCU applies.
	fsys.WriteFile("/etc/gocode/managed-settings.json", []byte(`{"tier":"etc"}`))
	ctx.OSPolicyReader = func() OSPolicyReadout {
		return OSPolicyReadout{IsWSL: true, HKLM: strp(`{"wslInheritsWindowsSettings":42}`),
			HKCU: strp(`{"tier":"hkcu","wslInheritsWindowsSettings":true}`)}
	}
	p = ComposePolicy(fsys, ctx)
	if p.Settings == nil || len(p.Settings) != 0 || p.FromHKCU {
		t.Fatalf("fail-closed policy must be an empty object: %+v", p)
	}
}

func TestDecodeXMLPlist(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>model</key><string>opus &amp; co</string>
  <key>n</key><integer>42</integer>
  <key>r</key><real>1.5</real>
  <key>on</key><true/>
  <key>perms</key><dict><key>deny</key><array><string>Bash(rm:*)</string></array></dict>
</dict></plist>`
	js, err := PlistToJSON([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(js), &got); err != nil {
		t.Fatalf("%v: %s", err, js)
	}
	want := map[string]any{"model": "opus & co", "n": 42.0, "r": 1.5, "on": true,
		"perms": map[string]any{"deny": []any{"Bash(rm:*)"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if _, err := PlistToJSON([]byte(`<plist><dict><key>d</key><date>2020-01-01T00:00:00Z</date></dict></plist>`)); err == nil {
		t.Fatal("<date> has no JSON form and must be rejected")
	}
	if _, err := PlistToJSON([]byte(`<plist><dict><key>k</key></dict></plist>`)); err == nil {
		t.Fatal("key without value must be rejected")
	}
}

// buildBPlist encodes a tiny bplist00 from pre-encoded objects (refSize 1).
func buildBPlist(objs [][]byte, top int) []byte {
	out := []byte("bplist00")
	var offs []byte
	for _, o := range objs {
		offs = append(offs, byte(len(out)))
		out = append(out, o...)
	}
	table := len(out)
	out = append(out, offs...)
	tr := make([]byte, 32)
	tr[6], tr[7] = 1, 1
	binary.BigEndian.PutUint64(tr[8:], uint64(len(objs)))
	binary.BigEndian.PutUint64(tr[16:], uint64(top))
	binary.BigEndian.PutUint64(tr[24:], uint64(table))
	return append(out, tr...)
}

func TestDecodeBinaryPlist(t *testing.T) {
	objs := [][]byte{
		{0xD3, 1, 2, 3, 4, 5, 6},             // 0: dict of 3 (keys 1,2,3; values 4,5,6)
		append([]byte{0x55}, "model"...),     // 1
		append([]byte{0x51}, "n"...),         // 2
		append([]byte{0x52}, "ok"...),        // 3
		{0x62, 0x00, 'h', 0x00, 'i'},         // 4: UTF-16 "hi"
		{0x11, 0x01, 0x00},                   // 5: int 256
		{0xA2, 7, 8},                         // 6: array [true, 1.5]
		{0x09},                               // 7
		{0x23, 0x3F, 0xF8, 0, 0, 0, 0, 0, 0}, // 8: real 1.5
	}
	js, err := PlistToJSON(buildBPlist(objs, 0))
	if err != nil {
		t.Fatal(err)
	}
	if js != `{"model":"hi","n":256,"ok":[true,1.5]}` {
		t.Fatalf("got %s", js)
	}

	cyc := [][]byte{{0xA1, 0}}
	if _, err := DecodePlist(buildBPlist(cyc, 0)); err == nil {
		t.Fatal("self-referencing array must be rejected")
	}
	if _, err := DecodePlist(buildBPlist([][]byte{{0x33, 0, 0, 0, 0, 0, 0, 0, 0}}, 0)); err == nil {
		t.Fatal("date must be rejected")
	}
	if _, err := DecodePlist([]byte("bplist00short")); err == nil {
		t.Fatal("truncated plist must be rejected")
	}
}

func TestPlistDocumentsFrom(t *testing.T) {
	files := map[string]string{
		"/a.plist": `<plist><dict><key>model</key><string>m</string></dict></plist>`,
		"/b.plist": `not a plist`,
	}
	docs := plistDocumentsFrom([]string{"/missing.plist", "/a.plist", "/b.plist"},
		func(p string) ([]byte, error) { return []byte(files[p]), nil },
		func(p string) bool { _, ok := files[p]; return ok })
	if len(docs) != 2 || docs[0].JSON == nil || *docs[0].JSON != `{"model":"m"}` ||
		docs[1].JSON != nil || docs[1].UnreadReason == "" {
		t.Fatalf("docs = %+v", docs)
	}
	p := ComposeOSPolicy(OSPolicyReadout{Plists: docs}, fsfacade.NewMemFS(), newCtx())
	if p.MDM.Settings["model"] != "m" {
		t.Fatalf("got %+v", p.MDM)
	}
}

func TestMacManagedPlistPathsOrder(t *testing.T) {
	got := MacManagedPlistPaths("bob")
	want := []string{
		"/Library/Managed Preferences/bob/" + MacManagedPreferencesDomain + ".plist",
		"/Library/Managed Preferences/" + MacManagedPreferencesDomain + ".plist",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if len(MacManagedPlistPaths("")) != 1 {
		t.Fatal("no per-user path without a user name")
	}
}
