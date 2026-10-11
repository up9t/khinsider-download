# Khinsider Scraper

## Todo

1. Make it work
2. Create Tests
3. Create Benchmark
4. Robust Telemetry

## TODO LATER LATER

1. Set a clear boundary to core logic
2. Compile the core logic it to wasm, and load it with javascript to then copy paste in the browser!!! ISN'T THAT REALLY COOL?


There are two versions:
1. Copy paste (simplest)

    Just like the name, copy paste the javascript code and paste it to the browser's console through developer tools.

2. Executable

    1. Download the binary
    2. Allow it to be executed

        ```bash
        chmod +x ./khd
        ```

    3. Run the binary
        ```bash
        ./khd <urls>

        # example
        # ./khd https://downloads.khinsider.com/game-soundtracks/album/plants-vs.-zombies
        ```

        > Note: It works with multiple urls too, just put them next to each other separated by space

    4. Check in the `downloads` directory in the current directory.
        ```bash
        ls ./downloads
        ```