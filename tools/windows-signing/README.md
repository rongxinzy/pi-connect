# Windows runtime signing

The release workflow authenticates with Certum SimplySign in the protected
`release` environment. Configure `CERTUM_USER_ID`, `CERTUM_OTP_URI` (the complete
TOTP URI), and `CERTUM_CERT_THUMBPRINT` as environment secrets. Do not copy these
credentials into desktop application repositories or unprotected PR workflows.

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
