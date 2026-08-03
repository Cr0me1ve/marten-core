package test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cr0me1ve/marten-core/v2/profile"
)

func TestAddByContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Profile-Title", "Marten Test")
		w.Header().Set("Subscription-Userinfo", "upload=0; download=0; total=1073741824; expire=1893456000")
		w.Header().Set("Support-Url", "https://support.example/help")
		w.Header().Set("Profile-Web-Page-Url", "https://profile.example")
		_, _ = w.Write([]byte(`{"outbounds":[{"type":"direct","tag":"out"}]}`))
	}))
	defer server.Close()

	entity, err := profile.AddByUrl(server.URL, "", false)
	if err != nil {
		t.Fatalf("expected no error, but got: %v", err)
	}
	defer profile.DeleteById(entity.Id)

	profileTitle := entity.Name
	expectedTitle := "Marten Test"
	if profileTitle != expectedTitle {
		t.Errorf("expected profile title to be %v, got %v", expectedTitle, profileTitle)
	}

	userInfo := entity.SubInfo
	if userInfo.Upload != 0 || userInfo.Download != 0 || userInfo.Total != 1073741824 || userInfo.Expire != 1893456000 {
		t.Errorf("subscription userinfo not parsed correctly, got: %v", userInfo)
	}

	supportURL := entity.SubInfo.SupportUrl
	if supportURL != "https://support.example/help" {
		t.Errorf("expected support URL to be https://support.example/help, got %v", supportURL)
	}

	profileWebPageURL := entity.SubInfo.WebPageUrl
	if profileWebPageURL != "https://profile.example" {
		t.Errorf("expected profile web page URL to be https://profile.example, got %v", profileWebPageURL)
	}
}
