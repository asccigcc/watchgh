// Renders the menu-bar icon from an SF Symbol into a black-on-transparent PNG
// (a template image — menuet forces image.template = true, and macOS then tints
// it for the light/dark menu bar automatically). Run via the Makefile's
// `make icon` target. Regenerate when changing the symbol or size.
//
// Usage: swift genicon.swift <symbol> <outdir>
//   writes <outdir>/menubar.png (22pt) and <outdir>/menubar@2x.png (44px)

import AppKit

func render(symbol: String, pointSize: CGFloat, canvas: CGFloat, to path: String) {
    let cfg = NSImage.SymbolConfiguration(pointSize: pointSize, weight: .medium)
    guard let base = NSImage(systemSymbolName: symbol, accessibilityDescription: nil),
          let sym = base.withSymbolConfiguration(cfg) else {
        FileHandle.standardError.write("could not load SF Symbol \(symbol)\n".data(using: .utf8)!)
        exit(1)
    }
    // Draw into an explicit 1x bitmap so pixel count == point count (a screen
    // lockFocus context would bake in the retina 2x scale and double the size).
    guard let rep = NSBitmapImageRep(
        bitmapDataPlanes: nil, pixelsWide: Int(canvas), pixelsHigh: Int(canvas),
        bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
        colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0) else {
        FileHandle.standardError.write("could not allocate bitmap\n".data(using: .utf8)!)
        exit(1)
    }
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
    let s = sym.size
    sym.draw(in: NSRect(x: (canvas - s.width) / 2, y: (canvas - s.height) / 2,
                        width: s.width, height: s.height))
    NSGraphicsContext.restoreGraphicsState()

    guard let png = rep.representation(using: .png, properties: [:]) else {
        FileHandle.standardError.write("could not encode PNG\n".data(using: .utf8)!)
        exit(1)
    }
    do {
        try png.write(to: URL(fileURLWithPath: path))
    } catch {
        FileHandle.standardError.write("write failed: \(error)\n".data(using: .utf8)!)
        exit(1)
    }
}

let args = CommandLine.arguments
let symbol = args.count > 1 ? args[1] : "eye"
let outdir = args.count > 2 ? args[2] : "."

// height-22 status-bar icon: ship @1x (22px) and @2x (44px) so AppKit reports a
// 22pt image and menuet skips its own resize, keeping the retina detail.
render(symbol: symbol, pointSize: 15, canvas: 22, to: "\(outdir)/menubar.png")
render(symbol: symbol, pointSize: 30, canvas: 44, to: "\(outdir)/menubar@2x.png")
print("wrote menubar.png + menubar@2x.png to \(outdir) from SF Symbol \"\(symbol)\"")
