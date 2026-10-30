package themes

// The modern palettes: the themes the full-screen "modern" UI style ships
// with, on top of the six classic palettes. Each is a full palette (the same
// token set as every other theme): a classic base palette with the tokens the
// modern look leans on re-tuned — the brand/accent pair that drives the
// gradients (gocode → gocodeShimmer), text and muted greys, borders (subtle,
// promptBorder), selection and tint backgrounds, status colors and diff bars.

// Modern theme names.
const (
	// ThemePrompty is the default dark theme: the mascot's greens for the
	// brand and accent roles on green-tinted neutral greys, with distinct
	// semantic colors (red errors, amber warnings, a separate color per
	// permission mode).
	ThemePrompty = "prompty"
	// ThemePromptyLight is the default light theme: Prompty's palette with
	// deeper greens that keep their contrast on a light background.
	ThemePromptyLight = "prompty-light"
	// ThemeMidnight is a modern dark theme: violet → pink accents on a cool
	// neutral grey.
	ThemeMidnight = "midnight"
	// ThemeDaylight is the modern light theme.
	ThemeDaylight = "daylight"
	// ThemeAurora is a dark theme with teal → green accents.
	ThemeAurora = "aurora"
	// ThemeEmber is a dark theme with warm amber → rose accents.
	ThemeEmber = "ember"
	// ThemeFrost is a dark theme with blue → cyan accents on slate.
	ThemeFrost = "frost"
	// ThemeMono is a near-greyscale dark theme with a single soft accent.
	ThemeMono = "mono"
)

// ModernThemeNames lists the modern themes, the default pair first.
var ModernThemeNames = []string{ThemePrompty, ThemePromptyLight, ThemeMidnight, ThemeDaylight, ThemeAurora, ThemeEmber, ThemeFrost, ThemeMono}

// IsModernTheme reports whether name is one of the modern palettes.
func IsModernTheme(name string) bool {
	for _, n := range ModernThemeNames {
		if n == name {
			return true
		}
	}
	return false
}

// modernOverrides are the per-theme token overrides applied to the base
// palette (light for the light themes, dark for the rest).
// composerSidebarBackground is the floating panels' surface (dialogs, the
// palette, menus, toasts): a step lighter than a dark terminal background (a
// step darker on daylight) so a panel reads as floating.
var modernOverrides = map[string]map[string]string{
	// Prompty: brand roles (title gradient, accent, prompt pointer, links,
	// selection, spinner, input frame) in the mascot's greens; the permission
	// modes keep their own hues (plan cyan, accept edits orchid, auto amber,
	// bypass red), distinct on 16-color terminals too, and diff additions use
	// a lime tone apart from the accent.
	ThemePrompty: {
		"gocode": "#34d27b", "gocodeShimmer": "#7cf5a6",
		"permission": "#8fe3c0", "permissionShimmer": "#b8f0d8",
		"suggestion": "#7cf5a6", "remember": "#7cf5a6",
		"autoAccept": "#c792ea", "autoAcceptShimmer": "#dcbdf2", "skill": "#c792ea", "merged": "#c792ea",
		"planMode": "#4dc8e0", "ide": "#74a9ff", "bashBorder": "#f78fc6",
		"text": "#e3ece6", "inverseText": "#0c1a11",
		"inactive": "#8b9c91", "inactiveShimmer": "#b4c3b9",
		"subtle": "#2e3d33", "promptBorder": "#4f8f68", "promptBorderShimmer": "#6fb088",
		"success": "#4ade80", "error": "#ff6b7d", "warning": "#e8c547", "warningShimmer": "#f2dc8a",
		"diffAdded": "#1f3a1a", "diffRemoved": "#4a1c26",
		"diffAddedDimmed": "#243320", "diffRemovedDimmed": "#3a2328",
		"diffAddedWord": "#a6e36e", "diffRemovedWord": "#ff7b8a",
		"userMessageBackground": "#1a2a20", "userMessageBackgroundHover": "#22352a",
		"composerSidebarBackground": "#18261d", "selectionBg": "#1f4a30",
		"bashMessageBackgroundColor": "#2b1d28", "memoryBackgroundColor": "#1a2833",
		"rate_limit_fill": "#34d27b", "rate_limit_empty": "#2e3d33",
		"effortUltra": "#c792ea", "briefLabelYou": "#74a9ff", "briefLabelgocode": "#34d27b",
		"clawd_body": "#188038",
		"mascotBody": "#188038", "mascotScreen": "#0b3d1d", "mascotFace": "#7cf5a6",
		"mascotHighlight": "#34d27b", "mascotFeet": "#0d652d", "mascotGreeting": "#7cf5a6",
	},
	ThemePromptyLight: {
		"gocode": "#137333", "gocodeShimmer": "#0b8043",
		"permission": "#0f7b62", "permissionShimmer": "#3d9a83",
		"suggestion": "#137333", "remember": "#137333",
		"autoAccept": "#a21caf", "autoAcceptShimmer": "#c050c8", "skill": "#a21caf", "merged": "#a21caf",
		"planMode": "#0369a1", "ide": "#1d4ed8", "bashBorder": "#be185d",
		"text": "#17211b", "inverseText": "#ffffff",
		"inactive": "#5f6e64", "inactiveShimmer": "#8a998f",
		"subtle": "#dde7e0", "promptBorder": "#6a9a7c", "promptBorderShimmer": "#8fb79d",
		"success": "#15803d", "error": "#b91c1c", "warning": "#757300", "warningShimmer": "#9a9733",
		"diffAdded": "#dcf5d0", "diffRemoved": "#fde0e4",
		"diffAddedDimmed": "#ebf6e5", "diffRemovedDimmed": "#f9ecee",
		"diffAddedWord": "#4d7c0f", "diffRemovedWord": "#b91c1c",
		"userMessageBackground": "#edf5ef", "userMessageBackgroundHover": "#e2efe6",
		"composerSidebarBackground": "#f1f6f2", "selectionBg": "#cfeedd",
		"bashMessageBackgroundColor": "#f8e6f0", "memoryBackgroundColor": "#e3eef9",
		"rate_limit_fill": "#137333", "rate_limit_empty": "#c4d6ca",
		"effortUltra": "#a21caf", "briefLabelYou": "#1d4ed8", "briefLabelgocode": "#137333",
		"clawd_body": "#188038",
		"mascotBody": "#188038", "mascotScreen": "#0b3d1d", "mascotFace": "#7cf5a6",
		"mascotHighlight": "#34d27b", "mascotFeet": "#0d652d", "mascotGreeting": "#137333",
	},
	ThemeMidnight: {
		"gocode": "#9d7cff", "gocodeShimmer": "#ff7ad9",
		"permission": "#8fa8ff", "permissionShimmer": "#b9c8ff",
		"suggestion": "#a996ff", "remember": "#a996ff",
		"autoAccept": "#c29bff", "autoAcceptShimmer": "#dcc6ff", "skill": "#c29bff",
		"planMode": "#4fd1c5", "ide": "#6cb6ff", "bashBorder": "#ff7ad9",
		"text": "#e6e6ef", "inverseText": "#14141c",
		"inactive": "#8a8ca3", "inactiveShimmer": "#b4b6c9",
		"subtle": "#3a3c4e", "promptBorder": "#5b5d75", "promptBorderShimmer": "#7d7f99",
		"success": "#4fdc9a", "error": "#ff6f8a", "warning": "#ffc66d", "warningShimmer": "#ffe0a3",
		"diffAdded": "#16382a", "diffRemoved": "#45192a",
		"diffAddedDimmed": "#1f3a2e", "diffRemovedDimmed": "#3a2229",
		"diffAddedWord": "#4fdc9a", "diffRemovedWord": "#ff6f8a",
		"userMessageBackground": "#1d1d2b", "userMessageBackgroundHover": "#262638",
		"composerSidebarBackground": "#1c1c2a", "selectionBg": "#352c63",
		"bashMessageBackgroundColor": "#2b1d2b", "memoryBackgroundColor": "#1b2633",
		"mascotBody": "#7c5ce6", "mascotScreen": "#1a1338", "mascotFace": "#d9ccff",
		"mascotHighlight": "#ff7ad9", "mascotFeet": "#5238a8", "mascotGreeting": "#d9ccff",
	},
	ThemeDaylight: {
		"gocode": "#6b4cf0", "gocodeShimmer": "#d6409f",
		"permission": "#4a63e0", "permissionShimmer": "#7488f0",
		"suggestion": "#5b45d8", "remember": "#5b45d8",
		"autoAccept": "#8442d6", "autoAcceptShimmer": "#a978e6", "skill": "#8442d6",
		"planMode": "#0f8a80", "ide": "#2a6fdb", "bashBorder": "#c42d86",
		"text": "#1d1d27", "inverseText": "#fbfbfe",
		"inactive": "#6e7086", "inactiveShimmer": "#9496aa",
		"subtle": "#d4d5e1", "promptBorder": "#a9abbd", "promptBorderShimmer": "#c4c6d4",
		"success": "#1a8f5a", "error": "#d03a58", "warning": "#a86a0c", "warningShimmer": "#c99434",
		"diffAdded": "#d5f5e3", "diffRemoved": "#fbdde3",
		"diffAddedDimmed": "#e6f5ec", "diffRemovedDimmed": "#f7e8eb",
		"diffAddedWord": "#1a8f5a", "diffRemovedWord": "#d03a58",
		"userMessageBackground": "#efeefa", "userMessageBackgroundHover": "#e5e3f7",
		"composerSidebarBackground": "#efeff6", "selectionBg": "#dcd5ff",
		"bashMessageBackgroundColor": "#f8e6f1", "memoryBackgroundColor": "#e4eef9",
		"mascotBody": "#6b4cf0", "mascotScreen": "#1d1442", "mascotFace": "#d4c8ff",
		"mascotHighlight": "#d6409f", "mascotFeet": "#4a2fb8", "mascotGreeting": "#5b45d8",
	},
	ThemeAurora: {
		"gocode": "#2dd4bf", "gocodeShimmer": "#a3e635",
		"permission": "#5eead4", "permissionShimmer": "#99f6e4",
		"suggestion": "#5eead4", "remember": "#5eead4",
		"autoAccept": "#a78bfa", "autoAcceptShimmer": "#c4b5fd", "skill": "#a78bfa",
		"planMode": "#38bdf8", "ide": "#60a5fa", "bashBorder": "#f472b6",
		"text": "#e2ece9", "inverseText": "#0b1513",
		"inactive": "#7f958f", "inactiveShimmer": "#a8bab5",
		"subtle": "#2c3d39", "promptBorder": "#4b625c", "promptBorderShimmer": "#6b847d",
		"success": "#4ade80", "error": "#fb7185", "warning": "#fbbf24", "warningShimmer": "#fcd34d",
		"diffAdded": "#123a2c", "diffRemoved": "#40202a",
		"diffAddedWord": "#4ade80", "diffRemovedWord": "#fb7185",
		"userMessageBackground": "#14231f", "userMessageBackgroundHover": "#1b2e29",
		"composerSidebarBackground": "#14231f", "selectionBg": "#1f4a42",
		"mascotBody": "#14a08f", "mascotScreen": "#062b27", "mascotFace": "#c4f27a",
		"mascotHighlight": "#a3e635", "mascotFeet": "#0c6e63", "mascotGreeting": "#c4f27a",
	},
	ThemeEmber: {
		"gocode": "#ff9f43", "gocodeShimmer": "#ff5e7e",
		"permission": "#ffb86b", "permissionShimmer": "#ffd3a1",
		"suggestion": "#ffa94d", "remember": "#ffa94d",
		"autoAccept": "#f78fb3", "autoAcceptShimmer": "#fbb6ce", "skill": "#f78fb3",
		"planMode": "#63cdda", "ide": "#74b9ff", "bashBorder": "#ff5e7e",
		"text": "#f1e9e4", "inverseText": "#1a1210",
		"inactive": "#9c8f88", "inactiveShimmer": "#c0b4ad",
		"subtle": "#3f3430", "promptBorder": "#64554f", "promptBorderShimmer": "#85756e",
		"success": "#7bd88f", "error": "#ff6b6b", "warning": "#ffd166", "warningShimmer": "#ffe29a",
		"diffAdded": "#1e3a26", "diffRemoved": "#4a1f22",
		"diffAddedWord": "#7bd88f", "diffRemovedWord": "#ff6b6b",
		"userMessageBackground": "#261c19", "userMessageBackgroundHover": "#302420",
		"composerSidebarBackground": "#241a17", "selectionBg": "#5a3324",
		"mascotBody": "#d9731f", "mascotScreen": "#3d1410", "mascotFace": "#ffc9d2",
		"mascotHighlight": "#ff5e7e", "mascotFeet": "#a4521c", "mascotGreeting": "#ffc9d2",
	},
	ThemeFrost: {
		"gocode": "#7aa2f7", "gocodeShimmer": "#7dcfff",
		"permission": "#89b4fa", "permissionShimmer": "#b4cdfb",
		"suggestion": "#89b4fa", "remember": "#89b4fa",
		"autoAccept": "#bb9af7", "autoAcceptShimmer": "#d4bdfa", "skill": "#bb9af7",
		"planMode": "#73daca", "ide": "#7dcfff", "bashBorder": "#f7768e",
		"text": "#dfe5f2", "inverseText": "#11141c",
		"inactive": "#7f8aa6", "inactiveShimmer": "#a9b2c8",
		"subtle": "#2f3549", "promptBorder": "#4c5573", "promptBorderShimmer": "#6b7594",
		"success": "#9ece6a", "error": "#f7768e", "warning": "#e0af68", "warningShimmer": "#ecc890",
		"diffAdded": "#1c3329", "diffRemoved": "#3f1f2b",
		"diffAddedWord": "#9ece6a", "diffRemovedWord": "#f7768e",
		"userMessageBackground": "#1a1f2e", "userMessageBackgroundHover": "#222839",
		"composerSidebarBackground": "#1a2030", "selectionBg": "#2e3c64",
		"mascotBody": "#4a78d8", "mascotScreen": "#0c1730", "mascotFace": "#7dcfff",
		"mascotHighlight": "#b4cdfb", "mascotFeet": "#2f529c", "mascotGreeting": "#7dcfff",
	},
	ThemeMono: {
		"gocode": "#c8c8d0", "gocodeShimmer": "#8fa8ff",
		"permission": "#b8bcc8", "permissionShimmer": "#d6d9e0",
		"suggestion": "#e0e0e6", "remember": "#e0e0e6",
		"autoAccept": "#b8bcc8", "autoAcceptShimmer": "#d6d9e0", "skill": "#b8bcc8",
		"planMode": "#9fb3c8", "ide": "#a8b0c0", "bashBorder": "#d0d0d8",
		"text": "#e4e4e8", "inverseText": "#141416",
		"inactive": "#85858f", "inactiveShimmer": "#aaaab2",
		"subtle": "#36363c", "promptBorder": "#57575f", "promptBorderShimmer": "#77777f",
		"success": "#a6d9b5", "error": "#e89aa5", "warning": "#e6cf96", "warningShimmer": "#efdfb5",
		"diffAdded": "#203027", "diffRemoved": "#3a2428",
		"diffAddedWord": "#a6d9b5", "diffRemovedWord": "#e89aa5",
		"userMessageBackground": "#1f1f23", "userMessageBackgroundHover": "#28282d",
		"composerSidebarBackground": "#1e1e23", "selectionBg": "#3a3a46",
		"mascotBody": "#8c8c96", "mascotScreen": "#1c1c21", "mascotFace": "#e4e4e8",
		"mascotHighlight": "#c8c8d0", "mascotFeet": "#5c5c64", "mascotGreeting": "#e4e4e8",
	},
}

// modernPalettes caches the built palettes (built once at init).
var modernPalettes = func() map[string]Palette {
	out := make(map[string]Palette, len(modernOverrides))
	for name, ov := range modernOverrides {
		base := defaultDarkPalette
		if IsLight(name) {
			base = lightPalette
		}
		out[name] = ApplyOverrides(base, ov)
	}
	return out
}()

// modernThemeOptions are the theme-picker labels for the modern palettes.
var modernThemeOptions = []ThemeOption{
	{Label: "Prompty (mascot green)", Value: ThemePrompty},
	{Label: "Prompty light (mascot green)", Value: ThemePromptyLight},
	{Label: "Midnight (modern dark)", Value: ThemeMidnight},
	{Label: "Daylight (modern light)", Value: ThemeDaylight},
	{Label: "Aurora (teal & lime)", Value: ThemeAurora},
	{Label: "Ember (amber & rose)", Value: ThemeEmber},
	{Label: "Frost (blue & cyan)", Value: ThemeFrost},
	{Label: "Mono (greyscale)", Value: ThemeMono},
}
