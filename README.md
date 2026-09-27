# 仓序 · 物品出入库管理

Go Gin + GORM + SQLite，前端原生 HTML/CSS/JavaScript 嵌入 Go 二进制，部署不依赖 Node.js。适配手机、平板和桌面。数据持久化到本地 SQLite 与图片目录，无外部云服务依赖。

## 功能

- 强制登录，管理员 / 普通用户权限、禁用账号、修改密码、会话过期、登录限流。
- 入库 / 出库必选，来源必填；操作时间可自行选择，不选时使用提交时间。支持条码相机识别、照片识别及手动输入条码 / 编码 / 序列号。
- JPG / PNG / GIF 图片上传及预览，提供「拍照上传」「从照片中选择」两个入口，上传大小及像素校验。
- 管理员维护分类；同一条码固定绑定名称、分类、单位；已有条码自动回填。
- 数量为正整数，出库不能超过库存；事务保证并发出库不会超卖。
- 记录按分类、方向、操作时间区间筛选及名称 / 条码 / 编码 / 序列号搜索；Excel 导出可选择起止日期时间并直接显示物品图片（文字导出最多 5 万条，含图导出最多 500 条带图记录）。
- 可在个人设置中填写姓名，管理员也可在账号管理中新增 / 修改姓名；记录和 Excel 的登记人优先显示姓名，留空时显示用户名。
- 软删除、回收站恢复、永久清空。删除或恢复会同步影响库存，阻止负库存操作。
- 库存查询、团队账号、操作日志。普通用户只能查看自己的流水，能查看全库物品与库存；管理员查看所有流水。
- HttpOnly / SameSite Cookie、CSRF 校验、bcrypt 密码存储、安全响应头。会话令牌只以哈希写入数据库。

## 本地启动

要求 Go 1.26+。使用纯 Go SQLite 驱动，无需安装 GCC 或 SQLite 服务。

PowerShell：

```powershell
$env:INVENTORY_ADMIN_PASSWORD = '在此设置至少12位的独立强密码'
go mod download
go run .
```

Linux / macOS：

```sh
export INVENTORY_ADMIN_PASSWORD='在此设置至少12位的独立强密码'
go mod download
go run .
```

访问 http://localhost:8080，账号默认 `admin`，密码为你设置的环境变量值。首次启动创建管理员；之后更改环境变量不会覆盖现有密码，请在页面右上角点击账号名修改密码。首次登录后先到「物品分类」新增分类，再登记物品。

```sh
go test ./...
go vet ./...
go build -trimpath -o inventory .
```

Windows 产物可命名 `inventory.exe`。二进制旁保留 `config.yaml`，启动时工作目录决定相对存储路径。前端已嵌入二进制，修改前端后需要重新构建。

Windows 发布压缩包解压后可双击 `start.cmd`，按提示设置首次启动密码；已有数据库可直接按回车。源码目录也提供相同启动脚本。

## 正式部署：Docker + Caddy HTTPS

部署前准备 Linux 服务器、Docker Compose，以及已指向服务器公网地址的域名。开放 80 / 443 端口。

1. 将项目复制到服务器。
2. `cp .env.example .env`，填写 `DOMAIN` 和独立强密码。DOMAIN 只写域名，如 `stock.example.com`。
3. 修改 `config.yaml` 的 `server.secure_cookie` 为 `true`。
4. `docker compose up -d --build`。
5. 通过 `https://你的域名` 访问。Caddy 自动申请与续期证书。

```sh
docker compose ps
docker compose logs --tail=100 inventory
```

应用端口不直接发布到公网。数据库和图片持久化在 `inventory-data` 卷，重建容器不丢失数据。不要使用 `docker compose down -v`，它会删除数据卷。正式环境建议限制服务器访问权限、定期备份。当前服务适合单实例小团队；不能把多个容器挂到同一个 SQLite 数据卷来横向扩容。

## 使用已有 Nginx 反向代理

如果出现「不允许跨站请求」，优先检查 Nginx 转发的 `Host`。例如浏览器访问 `https://stock.example.com`，而后端收到 `Host: 127.0.0.1:8080`，就会触发来源校验。非标准端口也必须保留。

在现有站点的 `location /` 中设置以下内容（完整片段见 `deploy/nginx.conf.example`）：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $http_host;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

这里使用 `$http_host`，以保留非标准端口；代理目标请按实际地址调整。先执行 `nginx -t`，成功后执行 `nginx -s reload`。上述 Host 修正对旧版程序也适用，无需升级或添加新配置项。不要清空或伪造 `Origin` 来绕过校验。

如果多层代理必须改写 Host，升级到支持 `allowed_origins` 的新程序后，在现有 `config.yaml` 的 `server` 下追加浏览器地址：

```yaml
server:
  address: ":8080"
  secure_cookie: true
  session_hours: 12
  allowed_origins:
    - "https://stock.example.com"
    # 非标准端口示例："https://stock.example.com:8443"
```

保留原有 storage、bootstrap 配置，并重启应用。填写实际协议、域名和端口，不填写登录路径；列表非空后仅允许其中的来源。默认为空时继续校验请求 Host。系统不会凭任意 `X-Forwarded-Host` 放行，登录后的写操作仍需 CSRF 令牌。HTTPS 部署设置 `secure_cookie: true`；HTTP 调试保持 `false`。

参考：[Nginx 官方代理请求头文档](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_set_header)。

## GitHub Actions 自动编译与发布

配置文件为 `.github/workflows/release.yml`。将整个项目（包括 `.github` 目录）推送到 GitHub 仓库后即可使用，无需额外配置个人访问令牌。

- 推送 `v1.0.0` 这样的版本标签：先运行测试、静态检查与漏洞扫描，再编译 Windows amd64、Linux amd64 / arm64、macOS amd64 / arm64，共 5 个平台包。
- 自动创建 GitHub Release，上传压缩包与 `SHA256SUMS.txt` 校验文件。每个包包含程序、默认配置、说明文档和第三方许可；Windows 包另含启动脚本。
- 标签含预发布后缀（如 `v1.0.0-rc.1`）时，创建预发布版本。
- Actions 页面选择 **Build and release → Run workflow** 可手动编译，产物保留 14 天；手动运行不会发布 Release。
- 所有平台编译成功才会进入发布。附件先上传到草稿，再公开版本；失败可重新运行。已经公开的版本不会覆盖，修改后请使用新的版本号。

首次发布示例（假定已配置 `origin`）：

```sh
git push origin main
git tag v1.0.0
git push origin v1.0.0
```

到仓库的 **Actions** 查看构建进度，完成后在 **Releases** 下载对应平台压缩包。发布作业使用 GitHub 自动提供的 `GITHUB_TOKEN`，只有该作业申请 `contents: write`；若组织策略禁止此权限，需由仓库管理员在组织 / 仓库设置中允许。

压缩包不包含数据库、图片、`.env` 或管理员密码。更新已部署系统时，请保留原有 `data/` 和 `config.yaml`，停止旧程序后替换可执行文件。此工作流发布可下载的程序，不负责远程服务器部署。macOS 产物未签名或公证。

Windows 解压后运行 `start.cmd`。Linux / macOS 解压后进入包目录，无需安装 Go，直接运行：

```sh
export INVENTORY_ADMIN_PASSWORD='请替换为至少12位的独立强密码'
./inventory
```

参考：[GitHub 构建产物说明](https://github.com/actions/upload-artifact)、[GitHub CLI Release 文档](https://cli.github.com/manual/gh_release_create)。

## 配置项

全部业务配置集中在 `config.yaml`：

| 字段 | 含义 |
| --- | --- |
| server.address | 监听地址，默认 :8080 |
| server.secure_cookie | HTTPS 正式部署必须 true；本机 HTTP 调试 false |
| server.session_hours | 登录有效期，1–168 小时，默认 12 |
| server.allowed_origins | 外部访问来源列表（协议、域名、端口）；为空时校验请求 Host |
| storage.database | SQLite 文件路径 |
| storage.uploads | 图片保存目录 |
| storage.max_upload_mb | 上传上限，1–32 MB |
| bootstrap.admin_username | 首次创建的管理员用户名 |
| bootstrap.admin_password | 备用初始密码，推荐通过环境变量提供 |

环境变量 `INVENTORY_CONFIG` 可指定另一个 YAML 文件，`INVENTORY_ADMIN_PASSWORD` 覆盖 YAML 中的初始化密码。真实密码、`.env`、本地覆盖配置及数据文件不要提交 Git。

## 使用规则与边界

- 条码 / 编码 / 序列号共用一个唯一标识字段。无条码物品可自定义，例如 `OFFICE-PAPER-A4`。同一标识不会允许填入不同名称、分类或单位，避免库存混淆；序列号按物品标识使用，不额外限制数量必须为 1。
- 来源填写实际来源、领用部门或调拨单位等信息，为必填文本。历史记录不会编造来源，显示「未填写（历史记录）」。
- 操作时间与系统登记时间分别保存。操作时间默认为提交时刻，也可补录历史日期；查询和导出区间均按操作时间筛选，区间端点包含所选时刻。补录时间不会改变按当前有效流水校验库存的规则，不进行历史时点库存重算。
- 升级启动时自动添加字段，旧记录（含回收站）的操作时间回填为原登记时间，姓名默认留空。升级前先备份整个数据目录，无需删除数据库或重新建库。
- 入出库数量为整数；统计卡累计数量可能包含不同单位，详细库存应按物品核对。
- 记录提交后不直接编辑。错误记录由管理员删除，再重新登记，以保留操作轨迹。
- 删除入库可能导致库存不足，此时应先处理对应出库。恢复出库也需要足够库存。
- 回收站清空只永久删除已经软删除的流水，不再重复扣减库存。物品档案、分类引用和审计日志继续保留。
- 图片文件在清空回收站后仍保留，避免误删共享或审计附件；未提交表单的上传也会保留。管理员可在确认备份后按数据库 Photo / Record 引用制定归档策略，当前不提供自动图片垃圾清理。
- 相机扫码需要 HTTPS 或 localhost，并允许摄像头权限。手机访问局域网 HTTP 地址时不能打开相机，可手动输入或选条码照片。支持常见 Code128、EAN、UPC、QR 等 ZXing 可识别格式；实际识别率取决于光线和设备。
- 页面时间使用浏览器本地时区，提交与筛选时转换为 UTC。Excel 按导出时浏览器的时区偏移显示，表头标明如 `UTC+08:00`；直接调用接口未传 `tz_offset` 则使用 UTC。Excel 中标识、名称、来源、备注以字符串写入，避免前导零丢失与公式注入，数量为可计算的数字。
- Excel 默认把图片缩成最多 160×120 像素的内嵌缩略图，离线打开也能看到，不依赖图片 URL。原图保留在系统。缺失或损坏的文件会注明「图片缺失或无法读取」。导出弹窗可取消包含图片，此时仅保留图片文件名。
- 手机的拍照入口使用系统相机调用方式（`capture=environment`）；桌面或不支持拍照调用的浏览器可能打开文件选择器。原生 HEIC 等格式请先转为 JPG / PNG。
- 当前不支持自助注册、邮件找回密码、多仓库、独立的序列号生命周期 / 批次管理、采购金额和小数数量。账号由管理员统一创建。姓名修改会更新历史记录的登记人显示，不修改账号 ID 或归属。
- 反向代理默认不信任客户端转发 IP，因此代理后限流按代理地址共享。如大量人员共同登录，应针对可信代理设计独立限流规则，切勿直接信任任意 X-Forwarded-For。

## 数据备份与恢复

最简单的可靠备份是短暂停止应用后同时复制数据库目录与图片目录，避免 WAL 和附件不一致。

本机：停止服务，将整个 `data/` 与 `config.yaml` 复制到带日期的安全备份目录，再启动。恢复时停止服务，把备份放回原路径并保留文件权限。密码与会话也包含在数据库中，备份需限制访问。

Docker（Linux）：

```sh
mkdir -p backups
docker compose stop inventory
docker compose run --rm --no-deps --user root --entrypoint sh -v "$PWD/backups:/backup" inventory -c 'tar -czf /backup/inventory-$(date +%Y%m%d-%H%M%S).tar.gz -C /app data'
docker compose start inventory
```

恢复到现有数据卷前先额外备份，并停止应用。确认备份文件后：

```sh
docker compose stop inventory
docker compose run --rm --no-deps --user root --entrypoint sh -v "$PWD/backups:/backup:ro" inventory -c 'tar -xzf /backup/你的备份文件.tar.gz -C /app && chown -R app:app /app/data'
docker compose start inventory
```

建议定期演练恢复；不要直接只复制正在写入的 `.db` 文件而忽略 `-wal`。

## 项目结构

```text
main.go             配置、ORM 模型、认证及业务接口
record_fields.go    操作时间迁移、姓名设置
export.go           时间区间导出与 Excel 内嵌图片
main_test.go        权限、库存、回收站、导出、上传集成测试
web/                嵌入式前端与本地扫码库
config.yaml         默认配置
Dockerfile          多阶段构建、非 root 运行
compose.yaml        应用与 HTTPS 反向代理
deploy/Caddyfile    Caddy 配置
```

接口全部在 `/api` 下，除 `/api/login` 外均需要 Cookie 会话。写操作需要 `/api/me` 返回的 `csrf` 放入 `X-CSRF-Token`。管理接口在 `/api/admin` 下。`/healthz` 用于健康检查。

扫码依赖固定为 `@zxing/browser 0.1.5`，本地分发的 UMD 包包含 ZXing 实现，许可见 `web/ZXING-LICENSE` 与 `web/ZXING-CORE-LICENSE`；不依赖公共 CDN。Go 依赖通过 `go.mod` 和 `go.sum` 锁定。
