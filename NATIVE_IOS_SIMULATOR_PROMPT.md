# Native iOS Cloud Build to Local Simulator Prompt

```text
你正在一台装有 Xcode 的办公 Mac 上开发 Native iOS 项目。编译在 SignCloud 的 macOS 构建机上完成，但 App 必须下载并安装到这台办公 Mac 自己的 iOS Simulator；不要使用或请求远程模拟器。

SignCloud 地址：
https://build.example.com

执行流程：

1. 检查当前目录中是否存在 .xcodeproj 或 .xcworkspace。
2. 使用 xcodebuild -list -json 自动确定共享 Scheme；存在 workspace 时优先使用 workspace。
3. 下载 SignCloud 客户端：

   mkdir -p "$HOME/.signcloud"
   curl -fsSL https://build.example.com/client/signcloud_ai.py -o "$HOME/.signcloud/signcloud_ai.py"
   chmod +x "$HOME/.signcloud/signcloud_ai.py"

4. 使用环境变量 SIGNCLOUD_API_TOKEN，不要要求我登录 Apple Developer。
5. 确认本机至少启动了一个 iOS Simulator；如果没有，打开 Simulator.app 并等待设备启动。
6. 从当前项目目录执行：

   python3 "$HOME/.signcloud/signcloud_ai.py" . \
     --kind simulator \
     --scheme "检测到的 Scheme" \
     --name "项目名-Simulator-时间" \
     --install-simulator

7. 该命令会把源码上传到 SignCloud，在云端构建 iphonesimulator 架构的 Debug .app.zip，然后下载到这台办公 Mac，执行 simctl install 和 simctl launch。
8. 构建失败时读取返回的云端 xcodebuild 日志，修改当前项目后重新提交。
9. 成功后向我汇报 Build ID、Bundle ID、版本、产物下载地址，以及安装和启动结果。

现在开始检查当前 Native iOS 项目并完成云编译及本地模拟器运行。
```
