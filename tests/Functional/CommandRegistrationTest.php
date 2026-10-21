<?php

declare(strict_types=1);

namespace App\Tests\Functional;

use Symfony\Bundle\FrameworkBundle\Console\Application;
use Symfony\Bundle\FrameworkBundle\Test\KernelTestCase;
use Symfony\Component\Console\Command\Command;

class CommandRegistrationTest extends KernelTestCase
{
    public function testUserManagementCommandsAreRegistered(): void
    {
        $application = new Application(self::bootKernel());

        $userAdd = $application->find('user:add');
        $inviteAdd = $application->find('invite:add');

        self::assertInstanceOf(Command::class, $userAdd);
        self::assertInstanceOf(Command::class, $inviteAdd);
    }
}
