# francetv

CLI tool to backup content from france.tv

## Usage

Use in your terminal, example:

`francetv --url https://www.france.tv/documentaires/animaux-nature/5166237-japon-un-nouveau-monde-sauvage.html`

Flags:

```
francetv
  -all
        Download all episodes if the page contains multiple videos.
  -debug
        Set debug mode
  -m3u8
        Should use HLS/m3u8 format to download (instead of dash)
  -subsOnly
        Only download the subtitles.
  -url string
        URL of the page to backup.
```

To backup all the episodes of a given show:

`francetv --url https://www.france.tv/france-4/c-est-toujours-pas-sorcier/toutes-les-videos/ --all`
