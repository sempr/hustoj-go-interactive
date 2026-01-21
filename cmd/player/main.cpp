#include <iostream>
#include <string>
#include <unistd.h>
#include <sys/types.h>

int main() {
    std::cerr << "Player debug - uid: " << getuid() << " pid: " << getpid() << " cwd: ";
    char cwd[1024];
    if (getcwd(cwd, sizeof(cwd)) != nullptr) {
        std::cerr << cwd;
    } else {
        std::cerr << "(unknown)";
    }
    std::cerr << std::endl;

    int low = 1;
    int high = 1000;

    while (low <= high) {
        int mid = (low + high) / 2;
        std::cout << mid << std::endl;
        std::cout.flush();

        std::string response;
        std::getline(std::cin, response);

        if (response == "correct") {
            return 0;
        } else if (response == "too small") {
            low = mid + 1;
        } else if (response == "too large") {
            high = mid - 1;
        }
    }

    return 0;
}
