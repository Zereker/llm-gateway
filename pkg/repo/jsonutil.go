package repo

// jsonOrNil returns nil for empty bytes (so MySQL JSON column gets NULL instead
// of failing on "" which is not valid JSON), otherwise the bytes as a string
// for the database driver to send as-is.
//
// Used by SQL impls when binding spec_detail / extra etc. to JSON columns.
func jsonOrNil(data []byte) any {
	if len(data) == 0 {
		return nil
	}
	return string(data)
}
