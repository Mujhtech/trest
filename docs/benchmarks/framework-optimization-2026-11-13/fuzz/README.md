# Differential fuzz verification

These successful runs total 1,743,257 fuzz executions: native JSON object decoding (324,883), native scalar decoding (602,464), parameter decoding (72,552), output encoding (373,832), unconstrained native decoding (113,866), and hexadecimal block encoding (255,660). Their logs are retained here.

The JSON and encoding tests compare supported native paths with the standard encoder/decoder plus the authoritative validator. Unsupported types and declined inputs keep the fallback path. Fuzzing is regression evidence, not a proof covering every input. The final integrated source also passed the race, vet, build, and lint checks linked by the main report.
