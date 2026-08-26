// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "SimpletonPackSwift",
    platforms: [.macOS(.v14)],
    products: [.executable(name: "simpleton-pack-swift", targets: ["SimpletonPackSwift"])],
    targets: [.executableTarget(name: "SimpletonPackSwift")]
)
