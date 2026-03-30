import Foundation
import AppKit
import UserNotifications

// cc-clip-notify: Minimal notification helper with image attachment support.
// Must run from within a .app bundle to provide CFBundleIdentifier.
//
// Usage: cc-clip-notify --title T --subtitle S [--body B] [--image /path/to/img]
//        cc-clip-notify --register  (request permission and wait for user response)

func parseArgs() -> (title: String, subtitle: String, body: String?, imagePath: String?, register: Bool) {
    let args = CommandLine.arguments
    var title = "cc-clip"
    var subtitle = ""
    var body: String? = nil
    var imagePath: String? = nil
    var register = false

    var i = 1
    while i < args.count {
        switch args[i] {
        case "--title" where i + 1 < args.count:
            i += 1; title = args[i]
        case "--subtitle" where i + 1 < args.count:
            i += 1; subtitle = args[i]
        case "--body" where i + 1 < args.count:
            i += 1; body = args[i]
        case "--image" where i + 1 < args.count:
            i += 1; imagePath = args[i]
        case "--register":
            register = true
        default:
            break
        }
        i += 1
    }
    return (title, subtitle, body, imagePath, register)
}

let params = parseArgs()

// NSApplication is needed for the notification permission dialog to appear
let app = NSApplication.shared
app.setActivationPolicy(.accessory)

let center = UNUserNotificationCenter.current()

func sendNotification() {
    center.requestAuthorization(options: [.alert]) { granted, error in
        guard granted else {
            if let e = error { fputs("auth error: \(e)\n", stderr) }
            else { fputs("notifications denied\n", stderr) }
            exit(1)
        }

        if params.register {
            fputs("notifications authorized\n", stderr)
            exit(0)
        }

        let content = UNMutableNotificationContent()
        content.title = params.title
        content.subtitle = params.subtitle
        if let body = params.body, !body.isEmpty {
            content.body = body
        }

        if let imgPath = params.imagePath {
            let url = URL(fileURLWithPath: imgPath)
            if let attachment = try? UNNotificationAttachment(
                identifier: "image",
                url: url,
                options: nil
            ) {
                content.attachments = [attachment]
            }
        }

        let id = "cc-clip-\(ProcessInfo.processInfo.globallyUniqueString)"
        let request = UNNotificationRequest(identifier: id, content: content, trigger: nil)
        center.add(request) { error in
            if let e = error { fputs("deliver error: \(e)\n", stderr) }
            // Give notification center time to process
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) {
                exit(0)
            }
        }
    }
}

DispatchQueue.main.async {
    sendNotification()
}

// Run the app event loop (needed for permission dialog)
app.run()
