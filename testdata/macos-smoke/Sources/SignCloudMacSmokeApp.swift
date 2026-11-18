import SwiftUI

@main
struct SignCloudMacSmokeApp: App {
    var body: some Scene {
        WindowGroup {
            Text(String(localized: "smoke.ready"))
                .padding(32)
        }
    }
}
