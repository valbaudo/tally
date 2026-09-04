#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define BUF_SIZE 32

static void maybe_universal_crash(const char *input) {
    if (strncmp(input, "PANIC", 5) == 0) {
        abort();
    }
}

static void process(const char *input) {
    char buf[BUF_SIZE];
    strcpy(buf, input);
    printf("processed %zu bytes, buf=\"%.8s...\"\n", strlen(input), buf);
}

int main(int argc, char **argv) {
    if (argc != 2) {
        fprintf(stderr, "usage: %s <pov-file>\n", argv[0]);
        return 2;
    }
    FILE *f = fopen(argv[1], "rb");
    if (!f) { perror("fopen"); return 2; }
    char input[4096];
    size_t n = fread(input, 1, sizeof(input) - 1, f);
    input[n] = '\0';
    fclose(f);

    maybe_universal_crash(input);
    process(input);
    return 0;
}
