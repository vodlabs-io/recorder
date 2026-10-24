package vlcli

// refuseForeignOwner has nothing to check on Windows.
func refuseForeignOwner(string) error { return nil }
