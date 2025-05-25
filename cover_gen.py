
from mediacovergenerator.style_multi_1 import create_style_multi_1
import base64
import sys

if __name__ == "__main__":
    library_name = sys.argv[1]
    zh_font_path = "justzerock-mp-plugin/fonts/multi_1_zh.ttf"
    en_font_path = "justzerock-mp-plugin/fonts/multi_1_en.ttf"
    res = create_style_multi_1(f"images/{library_name}", (library_name, None), (zh_font_path, en_font_path))
    if res:
        d = base64.b64decode(res)
        with open(f"images/{library_name}.png", "wb") as f:
            f.write(d)
