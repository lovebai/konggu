$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot
if (-not $env:INVENTORY_ADMIN_PASSWORD) {
    Write-Host '首次运行：请输入至少 12 位管理员密码。已有数据库可直接按回车。'
    $inventorySecurePassword = Read-Host -AsSecureString '管理员初始密码'
    $inventoryBstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($inventorySecurePassword)
    try { $env:INVENTORY_ADMIN_PASSWORD = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($inventoryBstr) }
    finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($inventoryBstr) }
}
Write-Host '启动后访问 http://localhost:8080；按 Ctrl+C 停止。'
if (Test-Path -LiteralPath './inventory.exe') {
    & './inventory.exe'
} elseif (Test-Path -LiteralPath './dist/inventory.exe') {
    & './dist/inventory.exe'
} else {
    go run .
}
if ($LASTEXITCODE -ne 0) { Read-Host '启动失败，请查看上方提示；按回车退出' }
