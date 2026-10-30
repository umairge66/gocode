package settingsload

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// OS-managed (MDM) settings tiers.
//
// The loader reads, once per process, the administrator policy the OS
// carries outside the managed-settings.json file:
//
//   - macOS managed preferences (MDM configuration profiles), delivered to the
//     loader as an ordered list of plist documents {label, stdout,
//     userWritable};
//   - the Windows registry policy keys HKLM\SOFTWARE\Policies\gocode and
//     HKCU\SOFTWARE\Policies\gocode, value "Settings" (a REG_SZ /
//     REG_EXPAND_SZ holding a JSON settings document);
//   - on WSL, the same Windows keys read through the Windows reg.exe, gated by
//     the wslInheritsWindowsSettings opt-in, plus the Windows managed folder
//     "C:\Program Files\gocode" seen through DrvFs.
//
// The policy chain
// is server-managed > MDM (plist / HKLM) > managed-settings.json (+ drop-ins)
// > HKCU. Server-managed settings come from gocode's hosted service and are
// not supported. ComposePolicy (policy.go) turns the rest into the effective
// policySettings source.
//
// Not supported, by design:
//   - the cowork-cloud "where asked" registry-tool trust check — cowork cloud
//     is a hosted surface;
//   - the managed-document link records,
//     which only feed diagnostics;
//   - telemetry on the OS read — nothing is sent anywhere.

// Registry/WSL constants.
const (
	RegistryKeyHKLM   = `HKLM\SOFTWARE\Policies\gocode`
	RegistryKeyHKCU   = `HKCU\SOFTWARE\Policies\gocode`
	RegistryValueName = "Settings"
	// RegistrySubkey is the key path below the hive.
	RegistrySubkey = `SOFTWARE\Policies\gocode`
	// registryQueryTimeoutMs is reg.exe's execFile timeout.
	registryQueryTimeoutMs = 5000
	// registryMaxBytes is reg.exe's stdout maxBuffer.
	registryMaxBytes = 2097152
	// WSLRegExe is the Windows reg.exe as seen from WSL.
	WSLRegExe = "/mnt/c/Windows/System32/reg.exe"
	// WSLWindowsManagedDir is C:\Program Files\gocode through DrvFs.
	WSLWindowsManagedDir = "/mnt/c/Program Files/gocode"
)

// registryUnreadOversize is the unread reason for an oversize registry value
// .
const registryUnreadOversize = "the value exceeds 2 MiB"

// managedControlKeys are the keys that steer how managed sources compose and
// so do not by themselves count as policy content.
var managedControlKeys = []string{"managedSourcesBehavior", "wslInheritsWindowsSettings"}

// adminOnlyKeys are honored only from administrator-controlled documents and
// are stripped from a user-writable one.
var adminOnlyKeys = []string{"managedMcpServers", "isolation", "attributionProjectPolicy"}

// pluginListKeys are likewise admin-only in a user-writable document
// .
var pluginListKeys = []string{"prependPlugins", "appendPlugins"}

// builtinPluginSuffix marks a built-in plugin id.
const builtinPluginSuffix = "@builtin"

// PlistDocument is one macOS managed-preferences document as handed to the
// composition. JSON is the document converted to JSON text;
// nil means it exists but could not be read/converted, with UnreadReason.
type PlistDocument struct {
	Label        string
	JSON         *string
	UnreadReason string
	UserWritable bool
}

// OSPolicyReadout is the raw OS read the composition consumes. The
// registry fields hold the already-extracted "Settings" value (from the
// reg.exe stdout); nil means absent.
type OSPolicyReadout struct {
	Plists           []PlistDocument
	HKLM             *string
	HKLMUnreadReason string
	HKCU             *string
	// IsWSL reports the WSL bridge applies.
	IsWSL bool
}

// ManagedDocument is a parsed OS policy document.
type ManagedDocument struct {
	Settings         map[string]any
	Errors           []settingspolicy.LoadError
	HasPolicyContent bool
	LoadState        settingspolicy.LoadState
	UserWritable     bool
}

// OSPolicy is the composed OS policy.
type OSPolicy struct {
	// MDM is the macOS-profile / HKLM tier. It sits above managed-settings.json.
	MDM ManagedDocument
	// HKCU is the per-user registry policy, the lowest admin tier.
	HKCU map[string]any
	// HKCUErrors carries diagnostics from the HKCU document.
	HKCUErrors []settingspolicy.LoadError
	// WSLInherits reports that WSL inherits the Windows policy chain, which
	// also adds the DrvFs Windows managed folder above /etc/gocode.
	WSLInherits bool
}

// regValueRE matches reg.exe's `    Settings    REG_SZ    <value>` line.
var regValueRE = regexp.MustCompile(`(?im)^[ \t]+` + regexp.QuoteMeta(RegistryValueName) + `[ \t]+REG_(?:EXPAND_)?SZ[ \t]+([\s\S]*)`)

// ExtractRegistryValue pulls the "Settings" string out of `reg query` output.
// ok is false when no such value line is present or it is empty.
func ExtractRegistryValue(stdout string) (string, bool) {
	m := regValueRE.FindStringSubmatch(stdout)
	if m == nil {
		return "", false
	}
	v := strings.TrimRight(m[1], " \t\r\n\v\f")
	if v == "" {
		return "", false
	}
	return v, true
}

// unreadDocumentError is the "could not be read" diagnostic for a managed
// document.
func unreadDocumentError(label, reason string, userWritable bool) settingspolicy.LoadError {
	sev := "fatal"
	if userWritable {
		sev = "warning"
	}
	return settingspolicy.LoadError{
		File:     label,
		Message:  "Managed settings document (" + label + ") could not be read: " + reason + "; none of its settings are in effect.",
		Severity: sev,
	}
}

// unparsableDocumentError is the not-a-JSON-object diagnostic.
func unparsableDocumentError(label string, userWritable bool) settingspolicy.LoadError {
	if userWritable {
		return settingspolicy.LoadError{
			File:     label,
			Message:  "Managed settings document (" + label + ") could not be parsed as a JSON object; none of its settings are in effect. Fix or remove it.",
			Severity: "warning",
		}
	}
	return settingspolicy.LoadError{
		File:     label,
		Message:  "Managed settings document could not be parsed as a JSON object; none of its settings are in effect. Fix or remove it.",
		Severity: "fatal",
	}
}

// hasPolicyContent reports whether s carries any key other than the
// composition-control keys.
func hasPolicyContent(s map[string]any) bool {
	for k := range s {
		if !containsString(managedControlKeys, k) {
			return true
		}
	}
	return false
}

// ParseManagedDocument parses one OS policy document's JSON text. A
// user-writable document (the HKCU key, a user-scope plist) has its admin-only
// keys stripped with a warning, and its errors are downgraded to warnings.
func ParseManagedDocument(content, label string, userWritable bool) ManagedDocument {
	var parsed any
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		parsed = nil
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return ManagedDocument{
			Settings:     map[string]any{},
			Errors:       []settingspolicy.LoadError{unparsableDocumentError(label, userWritable)},
			LoadState:    settingspolicy.LoadDidNotLoad,
			UserWritable: userWritable,
		}
	}
	var errs []settingspolicy.LoadError
	if userWritable {
		ignored := func(key string) {
			errs = append(errs, settingspolicy.LoadError{
				File: label, Path: key, Severity: "warning",
				Message: `"` + key + `" is only honored from administrator-controlled managed settings and was ignored in ` + label + `, which the user account can write.`,
			})
		}
		for _, k := range adminOnlyKeys {
			if _, has := obj[k]; has {
				delete(obj, k)
				ignored(k)
			}
		}
		for _, k := range pluginListKeys {
			if _, has := obj[k]; has {
				delete(obj, k)
				ignored(k)
			}
		}
		// enabledPlugins: only entries that DISABLE a non-built-in plugin
		// survive in a user-writable document.
		if ep, isObj := obj["enabledPlugins"].(map[string]any); isObj {
			kept := map[string]any{}
			dropped := false
			for id, v := range ep {
				if strings.HasSuffix(id, builtinPluginSuffix) || v != false {
					dropped = true
					continue
				}
				kept[id] = v
			}
			if dropped {
				if len(kept) > 0 {
					obj["enabledPlugins"] = kept
				} else {
					delete(obj, "enabledPlugins")
				}
				errs = append(errs, settingspolicy.LoadError{
					File: label, Path: "enabledPlugins", Severity: "warning",
					Message: `"enabledPlugins" entries that enable a plugin, or name a built-in, are only honored from administrator-controlled managed settings and were ignored in ` + label + `, which the user account can write.`,
				})
			}
		}
	}
	return ManagedDocument{
		Settings:         obj,
		Errors:           errs,
		HasPolicyContent: hasPolicyContent(obj),
		LoadState:        settingspolicy.LoadLoaded,
		UserWritable:     userWritable,
	}
}

// combineLoadState merges two load states.
func combineLoadState(a, b settingspolicy.LoadState) settingspolicy.LoadState {
	if a == settingspolicy.LoadDidNotLoad || b == settingspolicy.LoadDidNotLoad {
		return settingspolicy.LoadDidNotLoad
	}
	if a == settingspolicy.LoadLoaded || b == settingspolicy.LoadLoaded {
		return settingspolicy.LoadLoaded
	}
	return settingspolicy.LoadAbsent
}

// counts reports whether an admin document holds the slot: it has content or
// failed to load.
func (d ManagedDocument) counts() bool {
	return !d.UserWritable && (d.HasPolicyContent || d.LoadState == settingspolicy.LoadDidNotLoad)
}

// wslFlag is the wslInheritsWindowsSettings arming state.
type wslFlag string

const (
	wslArmed      wslFlag = "armed"
	wslDisarmed   wslFlag = "disarmed"
	wslUnreadable wslFlag = "unreadable"
)

// wslFlagOf reads wslInheritsWindowsSettings: true / "true" arms, false /
// "false" or absent disarms, anything else is unreadable.
func wslFlagOf(s map[string]any) wslFlag {
	v, ok := s["wslInheritsWindowsSettings"]
	if !ok || v == nil {
		return wslDisarmed
	}
	switch v {
	case true, "true":
		return wslArmed
	case false, "false":
		return wslDisarmed
	}
	return wslUnreadable
}

// wslFlagUnreadableError is the fail-closed diagnostic.
func wslFlagUnreadableError(file string) settingspolicy.LoadError {
	return settingspolicy.LoadError{
		File: file, Path: "wslInheritsWindowsSettings", Severity: "fatal",
		Message: `"wslInheritsWindowsSettings" in ` + file + ` holds a value that cannot be read (it takes true or false) and no Windows administrator policy is deployed beside it (HKLM or ` + WSLWindowsManagedDir + `), so whether WSL inherits Windows policy is unknown: no Windows or managed-file policy is in effect (/etc/gocode is not read beneath it), and unless server-managed settings supply the policy — they apply regardless — sign-in and policy enforcement fail closed until the flag is fixed.`,
	}
}

// wslFolderFlag is the result of scanning the DrvFs managed folder for the
// opt-in flag.
type wslFolderFlag struct {
	flag           wslFlag
	unreadablePath string
	records        []settingspolicy.LoadError
	loadState      settingspolicy.LoadState
}

// wslFlagFromManagedFolder scans managed-settings.json then the sorted
// managed-settings.d drop-ins of the Windows managed folder for
// wslInheritsWindowsSettings, stopping at the first armed document.
func wslFlagFromManagedFolder(fsys fsfacade.FS, dir string) wslFolderFlag {
	out := wslFolderFlag{flag: wslDisarmed, loadState: settingspolicy.LoadAbsent}
	check := func(path string) wslFlag {
		res := LoadFile(fsys, path)
		switch res.LoadState {
		case settingspolicy.LoadAbsent:
			return wslDisarmed
		case settingspolicy.LoadUnreadable:
			out.records = append(out.records, res.Errors...)
			out.loadState = settingspolicy.LoadDidNotLoad
			return wslDisarmed
		case settingspolicy.LoadUnparsable, settingspolicy.LoadMalformed:
			out.records = append(out.records, unparsableDocumentError(path, false))
			out.loadState = settingspolicy.LoadDidNotLoad
			return wslDisarmed
		}
		out.loadState = combineLoadState(out.loadState, settingspolicy.LoadLoaded)
		return wslFlagOf(res.Settings)
	}
	base := filepath.Join(dir, managedSettingsFile)
	out.flag = check(base)
	if out.flag == wslArmed {
		return out
	}
	if out.flag == wslUnreadable {
		out.unreadablePath = base
	}
	dropIn := filepath.Join(dir, managedDropInDir)
	if _, err := fsys.ReadDir(dropIn); err != nil {
		if !isNotExist(err) {
			out.records = append(out.records, settingspolicy.LoadError{
				File: dropIn, Severity: "fatal",
				Message: "Managed settings drop-in directory could not be read: " + err.Error(),
			})
			out.loadState = settingspolicy.LoadDidNotLoad
		}
		return out
	}
	for _, name := range dropInFiles(fsys, dropIn) {
		f := check(filepath.Join(dropIn, name))
		if f == wslArmed {
			out.flag = wslArmed
			return out
		}
		if f == wslUnreadable {
			out.flag = wslUnreadable
			if out.unreadablePath == "" {
				out.unreadablePath = filepath.Join(dropIn, name)
			}
		}
	}
	return out
}

// managedFilesPresent reports whether dir holds a managed-settings.json or
// drop-in that has policy content or could not be read/parsed.
func managedFilesPresent(fsys fsfacade.FS, dir string) bool {
	counts := func(path string) bool {
		res := LoadFile(fsys, path)
		switch res.LoadState {
		case settingspolicy.LoadAbsent:
			return false
		case settingspolicy.LoadLoaded:
			return hasPolicyContent(res.Settings)
		}
		return true
	}
	if counts(filepath.Join(dir, managedSettingsFile)) {
		return true
	}
	dropIn := filepath.Join(dir, managedDropInDir)
	if _, err := fsys.ReadDir(dropIn); err != nil {
		return !isNotExist(err)
	}
	for _, name := range dropInFiles(fsys, dropIn) {
		if counts(filepath.Join(dropIn, name)) {
			return true
		}
	}
	return false
}

// ComposeOSPolicy turns the raw OS read into the MDM / HKCU tiers, with this
// precedence:
//
//  1. plist documents in order: the first admin document with policy content
//     (or, for a user-writable one, any non-control key) is the MDM tier; a
//     user-writable document is skipped once an admin one held the slot.
//  2. HKLM "Settings". Native Windows: content makes it the MDM tier. On WSL
//     the Windows chain applies only when wslInheritsWindowsSettings is armed
//     in HKLM or in the DrvFs managed folder; otherwise HKLM is ignored.
//  3. An unreadable WSL flag fails closed: an empty MDM tier that holds the
//     slot.
//  4. HKCU "Settings" applies, as the lowest tier, only when no
//     managed-settings file is deployed (in the Windows folder when the WSL
//     chain is armed, or in the platform managed folder); on WSL it also
//     needs its own wslInheritsWindowsSettings:true (double opt-in). The two
//     control keys are dropped from it.
func ComposeOSPolicy(r OSPolicyReadout, fsys fsfacade.FS, ctx *Context) OSPolicy {
	var errs []settingspolicy.LoadError
	adminHeld := false
	state := settingspolicy.LoadAbsent

	for _, p := range r.Plists {
		if p.UserWritable && adminHeld {
			continue
		}
		var d ManagedDocument
		if p.JSON == nil {
			d = ManagedDocument{
				Settings:     map[string]any{},
				Errors:       []settingspolicy.LoadError{unreadDocumentError(p.Label, p.UnreadReason, p.UserWritable)},
				LoadState:    settingspolicy.LoadDidNotLoad,
				UserWritable: p.UserWritable,
			}
		} else {
			d = ParseManagedDocument(*p.JSON, p.Label, p.UserWritable)
		}
		wins := d.HasPolicyContent
		if d.UserWritable {
			wins = hasPolicyContent(d.Settings)
		}
		if wins {
			d.Errors = append(append([]settingspolicy.LoadError{}, errs...), d.Errors...)
			d.HasPolicyContent = true
			return OSPolicy{MDM: d}
		}
		adminHeld = adminHeld || d.counts()
		errs = append(errs, d.Errors...)
		if !d.UserWritable {
			state = combineLoadState(state, d.LoadState)
		}
	}

	hklmLabel := "Registry: " + RegistryKeyHKLM + `\` + RegistryValueName
	hkcuLabel := "Registry: " + RegistryKeyHKCU + `\` + RegistryValueName

	var hklm *ManagedDocument
	hklmFlag := wslDisarmed
	if r.HKLM != nil {
		d := ParseManagedDocument(*r.HKLM, hklmLabel, false)
		hklm = &d
		if d.LoadState == settingspolicy.LoadLoaded {
			hklmFlag = wslFlagOf(d.Settings)
		}
	} else if r.HKLMUnreadReason != "" {
		hklm = &ManagedDocument{
			Settings:  map[string]any{},
			Errors:    []settingspolicy.LoadError{unreadDocumentError(hklmLabel, r.HKLMUnreadReason, false)},
			LoadState: settingspolicy.LoadDidNotLoad,
		}
	}
	hklmHolds := hklm != nil && hklm.counts()
	if hklm != nil {
		errs = append(errs, hklm.Errors...)
		state = combineLoadState(state, hklm.LoadState)
	}

	flag := wslDisarmed
	flagFile := hklmLabel
	winDir := ctx.wslWindowsManagedDir()
	if r.IsWSL {
		flag = hklmFlag
		if flag != wslArmed {
			folder := wslFlagFromManagedFolder(fsys, winDir)
			if folder.flag == wslArmed || flag == wslDisarmed {
				flag = folder.flag
				flagFile = folder.unreadablePath
				if flagFile == "" {
					flagFile = winDir
				}
			}
			if flag == wslDisarmed {
				errs = append(errs, folder.records...)
				state = combineLoadState(state, folder.loadState)
			}
		}
	}
	inherits := flag != wslDisarmed
	base := ManagedDocument{Settings: map[string]any{}, Errors: errs, LoadState: state}

	if r.IsWSL && !inherits {
		// HKLM is not honored on WSL without the opt-in.
		return OSPolicy{MDM: base}
	}
	if hklm != nil && hklmHolds {
		return OSPolicy{MDM: *hklm, WSLInherits: inherits}
	}
	if flag == wslUnreadable {
		if !managedFilesPresent(fsys, winDir) {
			base.Errors = append(base.Errors, wslFlagUnreadableError(flagFile))
		}
		base.HasPolicyContent = true
		base.LoadState = combineLoadState(base.LoadState, settingspolicy.LoadLoaded)
		return OSPolicy{MDM: base, WSLInherits: inherits}
	}
	if r.HKCU != nil && !ctx.Diskless && !hkcuSuppressed(fsys, ctx, inherits) {
		d := ParseManagedDocument(*r.HKCU, hkcuLabel, true)
		if !r.IsWSL || d.Settings["wslInheritsWindowsSettings"] == true {
			s := make(map[string]any, len(d.Settings))
			for k, v := range d.Settings {
				if !containsString(managedControlKeys, k) {
					s[k] = v
				}
			}
			return OSPolicy{MDM: base, HKCU: s, HKCUErrors: d.Errors, WSLInherits: inherits}
		}
		if len(d.Errors) > 0 {
			return OSPolicy{MDM: base, HKCUErrors: d.Errors, WSLInherits: inherits}
		}
	}
	return OSPolicy{MDM: base, WSLInherits: inherits}
}

// hkcuSuppressed reports that a deployed managed-settings file outranks the
// user-writable HKCU policy.
func hkcuSuppressed(fsys fsfacade.FS, ctx *Context, inherits bool) bool {
	if inherits && managedFilesPresent(fsys, ctx.wslWindowsManagedDir()) {
		return true
	}
	return managedFilesPresent(fsys, ctx.ManagedBaseDir())
}

func (c *Context) wslWindowsManagedDir() string {
	if c.WSLWindowsManagedDirOverride != "" {
		return c.WSLWindowsManagedDirOverride
	}
	return WSLWindowsManagedDir
}

// readOSPolicy runs the context's OS reader, or the platform default (read
// once per process).
func (c *Context) readOSPolicy() OSPolicyReadout {
	if c.OSPolicyReader != nil {
		return c.OSPolicyReader()
	}
	return cachedPlatformOSPolicy()
}

// isNotExist tolerates ENOENT/ENOTDIR.
func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}
