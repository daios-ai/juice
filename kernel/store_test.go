package kernel

// Compile-time assertion: fakeStore must implement Store.
// This file ensures the interface and its fake remain in sync.
var _ Store = (*fakeStore)(nil)
