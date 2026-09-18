$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'runtime-authenticode.ps1')
$trusted = '0123456789ABCDEF0123456789ABCDEF01234567'
$script:signatureStatus = 'Valid'
$script:signerThumbprint = $trusted
$script:timestamp = [pscustomobject]@{ Subject = 'Timestamp authority' }
$script:eku = '1.3.6.1.5.5.7.3.3'
function Get-AuthenticodeSignature {
  param([string]$LiteralPath)
  return [pscustomobject]@{
    Status = $script:signatureStatus
    SignerCertificate = [pscustomobject]@{
      Thumbprint = $script:signerThumbprint
      Extensions = @([pscustomobject]@{
        Oid = [pscustomobject]@{ Value = '2.5.29.37' }
        EnhancedKeyUsages = @([pscustomobject]@{ Value = $script:eku })
      })
    }
    TimeStamperCertificate = $script:timestamp
  }
}
function Expect-Rejection {
  param([scriptblock]$Operation, [string]$Message)
  $rejected = $false
  try { & $Operation | Out-Null } catch {
    if ($_.Exception.Message -notlike "*$Message*") { throw }
    $rejected = $true
  }
  if (-not $rejected) { throw "Expected rejection: $Message" }
}
$fixture = $PSCommandPath
Assert-WindowsRuntimeSignature $fixture $trusted | Out-Null
Assert-WindowsRuntimeSignature $fixture $trusted.ToLowerInvariant() | Out-Null
Expect-Rejection { Assert-WindowsRuntimeSignature $fixture '' } 'RUNTIME_SIGNER_THUMBPRINT'
Expect-Rejection { Assert-WindowsRuntimeSignature $fixture ("z$trusted") } 'RUNTIME_SIGNER_THUMBPRINT'
Expect-Rejection { Assert-WindowsRuntimeSignature "$fixture.missing" $trusted } 'not found'
foreach ($status in @('NotSigned', 'HashMismatch', 'NotTrusted', 'UnknownError')) {
  $script:signatureStatus = $status
  Expect-Rejection { Assert-WindowsRuntimeSignature $fixture $trusted } 'expected Valid'
}
$script:signatureStatus = 'Valid'
$script:signerThumbprint = '1123456789ABCDEF0123456789ABCDEF01234567'
Expect-Rejection { Assert-WindowsRuntimeSignature $fixture $trusted } 'trusted certificate'
$script:signerThumbprint = $trusted
$script:timestamp = $null
Expect-Rejection { Assert-WindowsRuntimeSignature $fixture $trusted } 'trusted timestamp'
$script:timestamp = [pscustomobject]@{ Subject = 'Timestamp authority' }
$script:eku = '1.3.6.1.5.5.7.3.1'
Expect-Rejection { Assert-WindowsRuntimeSignature $fixture $trusted } 'code signing'
Write-Output 'Runtime Authenticode policy tests passed (12 cases).'
