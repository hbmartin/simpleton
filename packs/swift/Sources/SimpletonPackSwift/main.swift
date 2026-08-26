import Foundation

let protocolVersion = "1"

func response(id: Any, result: Any? = nil, error: [String: Any]? = nil) throws -> Data {
    var value: [String: Any] = ["jsonrpc": "2.0", "id": id]
    if let result { value["result"] = result }
    if let error { value["error"] = error }
    return try JSONSerialization.data(withJSONObject: value)
}

while let line = readLine() {
    guard let data = line.data(using: .utf8),
          let request = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
        continue
    }
    let id = request["id"] ?? NSNull()
    let method = request["method"] as? String ?? ""
    var result: Any?
    var rpcError: [String: Any]?
    switch method {
    case "initialize":
        let params = request["params"] as? [String: Any]
        if params?["protocol_version"] as? String != protocolVersion {
            rpcError = ["code": -32001, "message": "protocol version mismatch"]
        } else {
            result = ["capability": [
                "language": "swift",
                "pack_version": "0.1.0",
                "protocol_version": protocolVersion,
                "methods": ["analyze": false, "probe": false, "cancel": false, "types": false, "callers": false, "effects": false],
                "requirements": ["phase": "Linux Swift analysis follows the core packs; Apple execution follows in ephemeral macOS VMs"],
                "trust_classes": ["trusted_branch_linux_swift", "trusted_branch_apple_vm"]
            ]]
        }
    case "analyze":
        result = [
            "targets": [],
            "methods": [["id": "swift_native_analysis", "language": "swift", "status": "unsupported", "reason": "committed later delivery phase"]],
            "opportunities": []
        ]
    case "probe":
        result = [
            "method": ["id": "swift_native_probe", "language": "swift", "status": "unsupported", "reason": "committed later delivery phase"],
            "divergences": [], "replay_capsules": []
        ]
    case "cancel":
        result = ["cancelled": true]
    default:
        rpcError = ["code": -32601, "message": "method not found"]
    }
    if let encoded = try? response(id: id, result: result, error: rpcError),
       let text = String(data: encoded, encoding: .utf8) {
        print(text)
    }
}
