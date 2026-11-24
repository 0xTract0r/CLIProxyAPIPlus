package registry

// GetKiloModels returns the public model ID supported by the built-in Gateway adapter.
func GetKiloModels() []*ModelInfo {
	return []*ModelInfo{
		{
			ID: "kilo-auto/efficient", Object: "model", OwnedBy: "kilo", Type: "kilo",
			DisplayName: "Kilo Auto Efficient",
		},
	}
}
