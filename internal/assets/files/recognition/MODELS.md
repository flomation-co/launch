# Bundled recognition models

These weights are embedded in the Launch binary and served same-origin under
/recognition/* so in-browser recognition needs no third-party CDN.

- onnxruntime-web 1.20.1 — MIT License (Microsoft). Files: ort/ort.wasm.min.js,
  ort/ort-wasm-simd-threaded.mjs, ort/ort-wasm-simd-threaded.wasm.
- plate/detector.onnx — open-image-models "yolo-v9-t-384-license-plate-end2end"
  (github.com/ankandrew/open-image-models) — MIT License. Input images[1,3,384,384]
  float32 RGB /255 NCHW; output output0[N,7] = [img,x1,y1,x2,y2,cls,score] (end2end/NMS).
- plate/ocr.onnx — fast-plate-ocr "cct-xs-v2-global-model"
  (github.com/ankandrew/fast-plate-ocr) — MIT License. Input input[N,64,128,3]
  uint8 RGB NHWC; output plate[N,10,37] over alphabet "0-9A-Z_" (pad "_").
  Global model; regions include the United Kingdom.
