# Protocol bindings

Source definitions are under `anet/`; committed Go bindings are under `gen/`.
The current runtime implements the restricted profile in
[`services/rpc/README.md`](../services/rpc/README.md). A declared RPC does not imply
that its server implementation exists.

Generation pins: protoc **33.0**, protoc-gen-go **v1.36.10**,
protoc-gen-go-grpc **1.5.1**. Install the Go plugins into a dedicated directory:

```powershell
$env:GOBIN = (Join-Path (Get-Location) '.local/bin')
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
python proto/generate.py --protoc <protoc> --plugin-dir .local/bin
python proto/generate.py --protoc <protoc> --plugin-dir .local/bin --check
```

The generator checks all three versions. The protoc archive's `include/` directory
must be next to `bin/`, or supplied with `--include`. `--check` regenerates into a
temporary directory and rejects changed, missing or extra generated Go files.
For the local Windows run, `protoc-33.0-win64.zip` was verified against its official
release asset digest (`3742cd49c8b6bd78b6760540367eb0ff62fa70a1032e15dafe131bfaf296986a`).

`SignedEnvelope` carries a key ID, canonical signed payload and Ed25519 signature.
SubmitCommand requires the GW command and ICS root permit; AdmitEffect requires
the ICS decision; ReportEffectResult requires the adapter fact. Legacy unsigned
fields retain their wire numbers for compatibility review, but are rejected by
the implemented server profile. Caller identity is derived from mTLS registration.
