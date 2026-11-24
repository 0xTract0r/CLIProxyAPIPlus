package registry

import (
	"reflect"
	"testing"
)

func TestOrdinary852VertexNeonCatalogIsProviderScoped(t *testing.T) {
	vertex := LookupStaticModelInfoByChannel("gemini-4-neon", "vertex")
	if vertex == nil || vertex.Thinking == nil || vertex.ContextLength != 1048576 || vertex.MaxCompletionTokens != 65536 ||
		!reflect.DeepEqual(vertex.Thinking.Levels, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("Vertex Neon static model missing or malformed: %#v", vertex)
	}
	copilot := LookupStaticModelInfoByChannel("gemini-4-neon", "copilot")
	if copilot == nil || copilot.Thinking == nil || !reflect.DeepEqual(copilot.Thinking.Levels, []string{"low", "medium", "high"}) {
		t.Fatalf("Copilot Neon static thinking changed: %#v", copilot)
	}
}
