# Decodes the SVG codes written by `QR_DUMP=dir go test ./internal/qr -run TestDumpForIndependentDecoder` with OpenCV (pip install opencv-python-headless numpy):
# python3 scripts/qr_decode.py dir. Not part of the suite: it needs a decoder that is not in the repository.
import re, glob, sys
import numpy as np, cv2
ok = bad = 0
for f in sorted(glob.glob(sys.argv[1] + '/*.svg')):
    svg = open(f).read()
    n = int(re.search(r'viewBox="0 0 (\d+) \d+"', svg).group(1))
    img = np.full((n, n), 255, np.uint8)
    for x, y in re.findall(r'M(\d+) (\d+)h1v1h-1z', svg):
        img[int(y), int(x)] = 0
    scale = 8
    big = cv2.resize(img, (n * scale, n * scale), interpolation=cv2.INTER_NEAREST)
    want = open(f[:-4] + '.txt').read()
    # two detectors: the basic one is sensitive to the pixel size of a small code, the other is not
    got = cv2.QRCodeDetector().detectAndDecode(big)[0]
    if got != want:
        got = cv2.QRCodeDetectorAruco().detectAndDecode(big)[0]
    if got == want:
        ok += 1
    else:
        bad += 1
        print('MISMATCH', f.split('/')[-1], 'len', len(want), 'size', n, repr(got)[:60])
print('decoded correctly', ok, 'wrong or undecodable', bad)
