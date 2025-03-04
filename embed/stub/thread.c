#include <stdlib.h>
#include <pthread.h>

void* _lfi_thread_create(void* fn) {
    pthread_t* t = malloc(sizeof(pthread_t));
    pthread_attr_t attrs;

    int r = pthread_attr_init(&attrs);
    if (r) {
        return NULL;
    }

    r = pthread_attr_setstacksize(&attrs, 2 * 1024 * 1024);
    if (r) {
        return NULL;
    }
    pthread_create(t, &attrs, fn, NULL);
    return t;
}

void _lfi_thread_destroy(void* arg) {
    free((pthread_t*) arg);
}
