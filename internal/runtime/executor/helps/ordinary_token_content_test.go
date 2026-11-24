package helps

import (
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

func TestOrdinaryOpenAIContentResultAdapter(t *testing.T) {
	var segments []string
	CollectOpenAIContent(gjson.Result{Type: gjson.String, Str: "synthetic text"}, &segments)
	CollectOpenAIContent(gjson.Parse(`[{"type":"text","text":"array item"}]`), &segments)
	CollectOpenAIContent(gjson.Result{}, &segments)
	if want := []string{"synthetic text", "array item"}; !reflect.DeepEqual(segments, want) {
		t.Fatalf("token counting segments = %v, want %v", segments, want)
	}
}
