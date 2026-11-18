# SignCloud AI Prompt

将下面的 Prompt 发给办公电脑上的 Codex、Claude Code、Cursor Agent 或其他能执行终端命令的编程 AI。不要把 API Token 直接写进 Prompt；先在办公电脑的环境变量中设置。

## 办公电脑环境变量

PowerShell：

```powershell
$env:SIGNCLOUD_SERVER = "https://build.example.com"
$env:SIGNCLOUD_API_TOKEN = "由管理员单独提供的 API Token"
```

macOS/Linux：

```bash
export SIGNCLOUD_SERVER=https://build.example.com
export SIGNCLOUD_API_TOKEN='由管理员单独提供的 API Token'
```

将 `scripts/signcloud_ai.py` 放在办公电脑上的固定工具目录，或让 AI 使用其完整路径。

## 可复制 Prompt

```text
你现在需要使用我们的 SignCloud 服务完成 iOS 云编译、签名和打包。

约束：
1. 不要读取、打印、提交或写入源码任何 SIGNCLOUD_API_TOKEN；它只能从环境变量读取。
2. 先检查当前项目类型：Expo 项目使用 expo，原生 Objective-C/Swift/Xcode 项目使用 xcode，已有 IPA 需要重签名时使用 resign。
3. 编译前检查 bundle identifier、版本号、iOS deployment target、共享 Scheme，以及依赖锁文件是否完整。
4. 不要上传 .git、node_modules、DerivedData、build、dist 等生成目录。
5. 使用下面命令提交，PROJECT_PATH 替换为当前项目目录或 IPA 路径：

   Windows: python C:\path\to\signcloud_ai.py PROJECT_PATH --kind auto --name "项目名-版本号"
   macOS/Linux: python3 /path/to/signcloud_ai.py PROJECT_PATH --kind auto --name "项目名-版本号"

6. 如果项目需要指定 Scheme，追加：--scheme SchemeName
7. 如果构建失败，读取脚本输出的 SignCloud 构建日志，定位并修复项目问题，然后重新提交。不要通过关闭签名校验来绕过问题。
8. 成功后只向我汇报：Build ID、最终状态、安装页面地址、IPA 下载地址、Bundle ID 和版本号。
9. 如果任务长时间停留在 queued，明确告诉我 macOS Agent 未在线或没有领取任务，不要重复创建大量任务。
10. 如果我要安装到一台尚未登记的新 iPhone：
    - 只能在连接并信任该 iPhone 的 Mac 上执行；
    - 先运行 `python3 /path/to/signcloud_ai.py --list-local-devices`；
    - 输出只包含掩码 UDID，告诉我设备名称、型号和掩码 UDID，并在上传前取得我的明确确认；
    - 确认后运行 `python3 /path/to/signcloud_ai.py --enroll-device --device-index 1 --yes`；
    - 等待状态变成 registered，再用 `--export-method ad-hoc --standard-export` 重新构建；
    - 不要要求我把完整 UDID、Apple `.p8`、证书私钥或密码发到聊天中。
11. 进行 Ad Hoc 构建前，检查所有 iOS target 的签名设置。原项目 Bundle ID 如果已经属于其他 Apple Developer 团队，必须在工作副本中改成 SignCloud 团队可注册的全局唯一 ID，扩展 target 的 ID 也要同步调整；签名方式改为 Automatic，并清除项目中硬编码的 provisioning profile 和 signing identity。不要未经同意直接修改用户唯一的源码副本。

现在开始检查当前项目并执行云编译。
```

## 示例

```bash
python3 /path/to/signcloud/scripts/signcloud_ai.py . --kind auto --name "境外业务用户版-3.10.200"
```

仅提交、不等待：

```bash
python3 /path/to/signcloud/scripts/signcloud_ai.py . --kind expo --name "Expo Preview" --no-wait
```
