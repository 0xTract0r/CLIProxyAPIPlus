package cliproxy

// GetWatcher returns the underlying WatcherWrapper instance.
// This allows external components (e.g., RefreshManager) to interact with the watcher.
// Returns nil if the service or watcher is not initialized.
func (s *Service) GetWatcher() *WatcherWrapper {
	if s == nil {
		return nil
	}
	return s.watcher
}
