package translate

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/smithy-go/encoding/cbor"
)

func TestTranslateProviderOrderAndFallback(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/qwen/chat/completions":
			var input struct {
				Model    string
				Messages []chatMessage
				Options  map[string]string `json:"translation_options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || r.Header.Get("Authorization") != "Bearer test-token" {
				t.Errorf("invalid Qwen request: %+v, %v", input, err)
			}
			content := `{"detectedLanguage":{"language":"en","score":100},"translations":[{"to":"zh-Hant","text":"你好"},{"to":"ja","text":"こんにちは"}]}`
			if input.Options != nil {
				if input.Model != "qwen-mt-turbo" || input.Options["target_lang"] != "Japanese" || input.Options["source_lang"] != "auto" || len(input.Messages) != 1 {
					t.Errorf("invalid Qwen-MT request: %+v", input)
				}
				content = "MT result"
			} else if input.Model != "qwen-plus" || len(input.Messages) != 2 {
				t.Errorf("invalid Qwen request: %+v", input)
			}
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
		case "/azure/translate":
			if !reflect.DeepEqual(r.URL.Query()["to"], []string{"ja", "zh-Hant"}) || r.URL.Query().Get("api-version") != "3.0" ||
				r.Header.Get("Ocp-Apim-Subscription-Key") != "azure-key" || r.Header.Get("Ocp-Apim-Subscription-Region") != "eastasia" {
				t.Errorf("invalid Azure request: %s", r.URL)
			}
			w.Write([]byte(`[{"detectedLanguage":{"language":"en","score":0.96},"translations":[{"to":"ja","text":"Azure ja"},{"to":"zh-Hant","text":"Azure zh"}]}]`))
		case "/libre":
			var input struct{ Target, Source, Q string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Target != "ja" || input.Source != "auto" || input.Q != "hello" {
				t.Errorf("invalid LibreTranslate request: %+v, %v", input, err)
			}
			w.Write([]byte(`{"translatedText":"primary","alternatives":["first alternative"]}`))
		case "/service/AWSShineFrontendService_20170701/operation/TranslateText":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read AWS request: %v", err)
			}
			decoded, err := cbor.Decode(body)
			input, _ := decoded.(cbor.Map)
			if err != nil || input["Text"] != cbor.String("hello") || input["SourceLanguageCode"] != cbor.String("auto") ||
				!strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=aws-key/") ||
				r.Header.Get("Smithy-Protocol") != "rpc-v2-cbor" {
				t.Errorf("invalid AWS request: %+v, %v", input, err)
			}
			w.Header().Set("Content-Type", "application/cbor")
			w.Header().Set("Smithy-Protocol", "rpc-v2-cbor")
			switch input["TargetLanguageCode"] {
			case cbor.String("ja"):
				w.WriteHeader(http.StatusBadRequest)
				w.Write(cbor.Encode(cbor.Map{"__type": cbor.String("InvalidRequestException"), "message": cbor.String("test rejection")}))
			case cbor.String("zh-TW"):
				w.Write(cbor.Encode(cbor.Map{"SourceLanguageCode": cbor.String("en"), "TargetLanguageCode": cbor.String("zh-TW"), "TranslatedText": cbor.String("AWS zh")}))
			default:
				t.Errorf("unexpected AWS target %v", input["TargetLanguageCode"])
			}
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	t.Cleanup(mock.Close)
	all := Config{Qwen: true, Azure: true, Libre: true, AWS: true, MachineTranslation: true,
		OpenAIURL: mock.URL + "/qwen", OpenAIKey: "test-token", AzureURL: mock.URL + "/azure", AzureKey: "azure-key",
		LibreURL: mock.URL + "/libre", AWSURL: mock.URL, AWSKey: "aws-key", AWSSecret: "aws-secret",
	}
	request := Request{Text: "hello", To: []string{"ja", "zh-Hant"}}
	check := func(config Config, req Request, want []Item, score float64) {
		t.Helper()
		result := New(config).Translate(t.Context(), req)
		if !reflect.DeepEqual(result.Translations, want) {
			t.Fatalf("translated %+v, want %+v", result.Translations, want)
		}
		if score == 0 && result.DetectedLanguage != nil || score != 0 && (result.DetectedLanguage == nil || result.DetectedLanguage.Score != score) {
			t.Fatalf("detected language = %+v, want score %v", result.DetectedLanguage, score)
		}
	}
	check(all, request, []Item{{"こんにちは", "ja"}, {"你好", "zh-Hant"}}, 100)
	failed := all
	failed.OpenAIURL = mock.URL + "/unavailable"
	check(failed, request, []Item{{"hello", "ja"}, {"hello", "zh-Hant"}}, 0)
	forced := request
	force := true
	forced.LLM = &force
	check(all, forced, []Item{{"MT result", "ja"}}, 0)
	all.Qwen = false
	check(all, request, []Item{{"Azure ja", "ja"}, {"Azure zh", "zh-Hant"}}, 0.96)
	all.Azure = false
	check(all, request, []Item{{"first alternative", "ja"}}, 0)
	all.Libre = false
	check(all, request, []Item{{"hello", "ja"}, {"AWS zh", "zh-Hant"}}, 0)
	all.AWS = false
	check(all, request, []Item{{"MT result", "ja"}}, 0)
	check(Config{}, request, []Item{{"hello", "ja"}, {"hello", "zh-Hant"}}, 0)
	check(all, Request{Text: "hello", To: []string{}}, []Item{}, 0)
}
