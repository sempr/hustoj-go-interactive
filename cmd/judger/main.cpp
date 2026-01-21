#include <iostream>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

void report(const char* s) {
    write(3, s, strlen(s));
    write(3, "\n", 1);
}

int main() {
    std::cerr << "Judge debug - uid: " << getuid() << " pid: " << getpid() << " cwd: ";
    char cwd[1024];
    if (getcwd(cwd, sizeof(cwd)) != nullptr) {
        std::cerr << cwd;
    } else {
        std::cerr << "(unknown)";
    }
    std::cerr << std::endl;

    const int secret = 731;
    int x;

    for (int i = 0; i < 10; i++) {
        if (!(std::cin >> x)) {
            report("{\"status\":\"RE\",\"reason\":\"bad input\"}");
            return 0;
        }
        if (x < secret) {
            std::cout << "too small" << std::endl;
        } else if (x > secret) {
            std::cout << "too large" << std::endl;
        } else {
            std::cout << "correct" << std::endl;
            report("{\"status\":\"AC\"}");
            return 0;
        }
    }
    report("{\"status\":\"WA\",\"reason\":\"limit\"}");
    return 0;
}

