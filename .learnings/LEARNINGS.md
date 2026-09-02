## [LRN-20260903-001] exclude-colocated-tests-from-production-marker-audit
Priority: medium
Area: tests
Summary: Production completeness scans must explicitly exclude colocated Go test files.
Details: The governance scanner already skipped tests/ directories, but tunnel-client Go tests live beside production packages and legitimately contain marker text in assertions and test fixtures. Treating every *_test.go file as production code created false blocking findings.
Suggested Action: Keep test-file exclusion narrowly scoped to *_test.go while continuing to scan production source and configuration files for incomplete markers.
