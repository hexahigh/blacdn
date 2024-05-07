# Very ~~good~~ docs

## Parameters

| Name  | Formats | Type     | Description           |
| ----- | ------- | -------- | --------------------- |
| `url` | `*`     | `string` | The URL of the image. |
| `f`   | `*`     | `string` | The format of the image. |
| `w`   | `*`     | `float`  | The width of the image. |
| `h`   | `*`     | `float`  | The height of the image. |
| `q`   | `*`     | `int`    | The quality of the image. |
| `c`   | `png, webp, avif, jxl`     | `int`    | Compression/effort level for the image. |
| `l`   | `webp, avif, jxl`     | `bool`    | Lossless mode |
| `s`   | `*`     | `bool`    | Strip metadata from the image. |

## Formats

- `png`
- `jpg`
- `webp`
- `avif`
- `jxl`


## Notes:
- Bool values should be provided like this `l=1`, for example, `http://localhost:8080/img?url=...&l=1`.
- The max compression level varies from format to format, setting the value too high will result in an error.