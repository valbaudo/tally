#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define BUF_SIZE 32

/* A synthetic "always crashes" marker, present in BOTH builds.
 * Exists only so the oracle's soundness can be tested: a PoV that
 * trips this is NOT evidence the fix works, and the oracle must
 * reject it. */
static void maybe_universal_crash(const char *input) {
    if (strncmp(input, "PANIC", 5) == 0) {
        abort();
    }
}

static void process(const char *input) {
    char buf[BUF_SIZE];
#ifdef FIXED
    strncpy(buf, input, BUF_SIZE - 1);
    buf[BUF_SIZE - 1] = '\0';
#else
    strcpy(buf, input); /* BUG: unbounded copy into fixed-size stack buffer */
#endif
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
