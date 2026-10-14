package main

import "testing"

const propfindOK = `<?xml version="1.0" encoding="UTF-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:response>
    <d:href>/remote.php/dav/spaces/abc$def/spike/f1.txt</d:href>
    <d:propstat>
      <d:prop><d:getetag>"1f2e3d4c"</d:getetag></d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
  </d:response>
</d:multistatus>`

const propfindWithEmptyFirstPropstat = `<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:propstat><d:prop></d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat>
    <d:propstat><d:prop><d:getetag>"zz"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  </d:response>
</d:multistatus>`

func TestParseETagKeepsQuotes(t *testing.T) {
	got, err := parseETag([]byte(propfindOK))
	if err != nil || got != `"1f2e3d4c"` {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestParseETagSkipsEmptyPropstat(t *testing.T) {
	got, err := parseETag([]byte(propfindWithEmptyFirstPropstat))
	if err != nil || got != `"zz"` {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestParseETagErrors(t *testing.T) {
	if _, err := parseETag([]byte(`<d:multistatus xmlns:d="DAV:"></d:multistatus>`)); err == nil {
		t.Error("expected an error when there is no etag")
	}
	if _, err := parseETag([]byte(`not xml`)); err == nil {
		t.Error("expected an error for invalid XML")
	}
}
