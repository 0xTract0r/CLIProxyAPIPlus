package auth

// OAuth alias publication and route selection share the ordinary plan gate.
func authAllowsRouteModel(auth *Auth, model string) bool {
	return auth == nil || schedulerModelEntitled(auth, canonicalModelLookupKey(model))
}
