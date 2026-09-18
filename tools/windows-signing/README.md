# Windows runtime signing

Certum credentials remain exclusively in RongxinAI's protected `release`
environment. This repository must not configure Certum credentials or
authenticate to SimplySign. Tagged builds only upload unsigned Actions
artifacts. Final signed releases are published in this original repository.

Configure `RUNTIME_ARTIFACT_READ_TOKEN` for Actions/read and Contents/read in
RongxinAI and Contents/read in this repository. Configure the public
`RUNTIME_SIGNER_THUMBPRINT` variable and protect the `release` environment.
No cross-repository write permission or Certum secret is required here.

After a successful tagged build, dispatch RongxinAI's central signing workflow
from main with its build run ID. Then dispatch this repository's release
workflow from main with the successful central `signing_run_id`.
Publication verifies both workflow origins, main ancestry, unchanged tag,
manifest identity/hashes and actual Authenticode before creating a release.
Artifacts expire after 14 days; expired inputs require a new build/sign run.
See the [central operational guide](https://github.com/rongxinzy/RongxinAI/blob/main/scripts/runtime-signing/README.md)
for manual stages, least-privilege credentials and recovery details.

Windows executables are signed with SHA-256 and an RFC 3161 timestamp before
archives, release checksums, or SBOMs are generated. Signing and verification
fail closed; there is no unsigned release fallback. Source tags must belong
to main. Publish a new immutable tag after merging this change, rather than
replacing assets under an existing tag.

Desktop consumers must update the pinned release and SHA-256 values together.
Configure their public `RUNTIME_SIGNER_THUMBPRINT` repository variable to match
the runtime certificate. Consumers verify trust, certificate identity,
code-signing usage, and timestamp without access to the cloud private key.

Policy tests use mocked signature results, not a cloud key:

```powershell
go test ./tools/windows-signing
```

These tests cover absent/invalid identity, missing executables, unsigned,
tampered, untrusted, or unknown signatures, wrong signers, missing timestamps,
and wrong certificate usages. They do not prove the cloud credentials work;
the first protected signed release is required for that verification.
