package chromehost

import "testing"

// Fixed key -> id vector. The key is the base64 SPKI DER public key committed
// in extension/manifest.json; the id is what Chrome derives for the unpacked
// extension and what `chrome install` pins into allowed_origins. If the
// committed key changes, both this vector and the manifest must change together.
const (
	testManifestKey = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAweaAPDU0d7iUk3uB1Ba555IxVIlvoe3ElgwfeCS0BBDJvwGPshOSUJ5vmA3W8TlK+EKEQytMerIIwFRWp5ofBkChnhVxj1sLRg1gtBAa9BExiRgMxJlK/Y4+5qYlpE340wMTwfs5E3b59uHZN1yo8lw3/Z9WoSJ+YJZhG5LtnCqz6mzHh4cz0mF6MjXrlalsin8UN8BhHlopT7GPC1q4EuIAk/yxbXqodU29RKeIApM10N+aklBhj/ogkdd7tbprkhh0SZPDGIOKsqJcmVvTb92jlcOcfiWfIdsjcLcuA4S7PJ9kHx0rMbAMzXAuZE8k2ppLBEOGxGCwkDg+j5AInwIDAQAB"
	testExtensionID = "enlbkekddkndgijidajppabineoekbfh"
)

func TestExtensionIDFromKey_KnownVector(t *testing.T) {
	id, err := ExtensionIDFromKey(testManifestKey)
	if err != nil {
		t.Fatalf("ExtensionIDFromKey: %v", err)
	}
	if id != testExtensionID {
		t.Fatalf("id = %q, want %q", id, testExtensionID)
	}
	if len(id) != 32 {
		t.Fatalf("id length = %d, want 32", len(id))
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 'a' || id[i] > 'p' {
			t.Fatalf("id char %q out of [a-p] range", id[i])
		}
	}
}

func TestExtensionIDFromKey_BadBase64(t *testing.T) {
	if _, err := ExtensionIDFromKey("not!base64!"); err == nil {
		t.Fatal("expected error for invalid base64 key")
	}
}

// An --extension-id is pinned verbatim into allowed_origins; anything but a
// real 32-char [a-p] id (a wildcard, a second origin, a path) is refused.
func TestValidExtensionID(t *testing.T) {
	good := ExtensionIDFromDER([]byte("x"))
	if !ValidExtensionID(good) {
		t.Fatalf("rejected derived id %q", good)
	}
	for _, bad := range []string{"", "*", good + "/", good[:31], good[:31] + "z", "chrome-extension://" + good, good[:16] + "/../" + good[:12]} {
		if ValidExtensionID(bad) {
			t.Fatalf("accepted bad id %q", bad)
		}
	}
}
